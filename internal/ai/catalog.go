package ai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Suggestion is one explanation + fix prompt. FixPrompt is the base prompt;
// ForAgent applies the founder's agent style when it is shown or emailed.
type Suggestion struct {
	Origin        string // catalog or agent
	CatalogKey    string
	Severity      string // low, medium, high
	Title         string
	WhatBroke     string
	WhoIsAffected string
	Why           string // empty when the founder's agent has to find out
	FixPrompt     string
	HowToCheck    string
	Confidence    float64
}

type entry struct {
	key   string
	match func(c *Context) bool
	build func(c *Context) Suggestion
}

func postgrestExtras(body string) (details, hint string) {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return "", ""
	}
	if s, ok := m["details"].(string); ok {
		details = s
	}
	if s, ok := m["hint"].(string); ok {
		hint = s
	}
	return details, hint
}

func (c *Context) net() *NetSample {
	if len(c.Net) == 0 {
		return nil
	}
	return &c.Net[0]
}

func (c *Context) js() *JSSample {
	if len(c.JS) == 0 {
		return nil
	}
	return &c.JS[0]
}

func (c *Context) page() string {
	if c.Route == "" {
		if s := c.srv(); s != nil {
			// A server error outside a web request: a background job.
			if s.Transaction != "" {
				return "the background task `" + s.Transaction + "`"
			}
			return "a background job"
		}
	}
	switch c.Route {
	case "", "/":
		return "the home page (/)"
	}
	return "the " + c.Route + " page"
}

func pgCode(c *Context, codes ...string) bool {
	n := c.net()
	if n == nil || n.Backend != "postgrest" {
		return false
	}
	for _, code := range codes {
		if n.Code == code {
			return true
		}
	}
	return false
}

func authCode(c *Context, codes ...string) bool {
	n := c.net()
	if n == nil || n.Backend != "supabase_auth" {
		return false
	}
	for _, code := range codes {
		if n.Code == code {
			return true
		}
	}
	return false
}

func (c *Context) severity(base string) string {
	if base == "low" {
		return "low"
	}
	if c.KeyPage || c.Users >= 3 || base == "high" {
		return "high"
	}
	return "medium"
}

func tableOr(c *Context, fallback string) string {
	if n := c.net(); n != nil {
		if t := TableFromPath(n.Path); t != "" {
			return t
		}
	}
	return fallback
}

func opOf(method string) (verb, noun string) {
	switch method {
	case "POST":
		return "add", "adding rows to"
	case "PATCH", "PUT":
		return "update", "updating rows in"
	case "DELETE":
		return "delete", "deleting rows from"
	}
	return "read", "reading"
}

var (
	reVarcharLimit = regexp.MustCompile(`character varying\((\d+)\)`)
	reRLSTable     = regexp.MustCompile(`for table "([^"]+)"`)
	reHintTable    = regexp.MustCompile(`'(?:public\.)?([\w]+)'`)
	reDetailKey    = regexp.MustCompile(`Key \(([\w, ]+)\)`)
	reFKTable      = regexp.MustCompile(`not present in table "([^"]+)"`)
	reNotNull      = regexp.MustCompile(`null value in column "([^"]+)" of relation "([^"]+)"`)
	reBadInput     = regexp.MustCompile(`invalid input syntax for type (\w+): "([^"]*)"`)
	reLocalHost    = regexp.MustCompile(`^(localhost|127\.|10\.|192\.168\.|0\.0\.0\.0|\[::1\])`)
	reNotFunction  = regexp.MustCompile(`([\w$.]+) is not a function`)
	reNotDefined   = regexp.MustCompile(`([\w$]+) is not defined`)
)

// baseCatalog is the original catalog (Phase 4); catalog_more.go assembles
// the full, ordered catalog around it.
var baseCatalog = []entry{
	{
		key:   "pg.value_too_long",
		match: func(c *Context) bool { return pgCode(c, "22001") },
		build: func(c *Context) Suggestion {
			n, table := c.net(), tableOr(c, "the table")
			limit := "a fixed number of"
			if m := reVarcharLimit.FindStringSubmatch(n.Message); m != nil {
				limit = m[1]
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("Saving to %s fails when a value is longer than %s characters", table, limit),
				WhatBroke:  fmt.Sprintf("When someone submits data on %s that goes into the `%s` table, saving fails because one value is longer than the database allows (%s characters). Nothing gets saved.", c.page(), table, limit),
				Why:        fmt.Sprintf("A text column in `%s` was created with a %s-character limit (`character varying(%s)`), which is too short for real data such as email addresses or names.", table, limit, limit),
				FixPrompt:  fmt.Sprintf("In the Supabase table `%s`, a text column is limited to %s characters (type `character varying(%s)`), and saving longer values fails with error 22001 \"%s\". It happens when the app saves data on %s.\n\nFind the column in `%s` whose type is `varchar(%s)`, most likely the one the form on %s fills in (for example an email or name field), and change its type to `text` with a new migration. Don't change any other column or table.", table, limit, limit, n.Message, c.page(), table, limit, c.page()),
				HowToCheck: fmt.Sprintf("On %s, submit the form with a value longer than %s characters (for example a 40-character email address). It should save without an error.", c.page(), limit),
				Confidence: 0.85,
			}
		},
	},
	{
		key:   "pg.rls_denied",
		match: func(c *Context) bool { return pgCode(c, "42501") },
		build: func(c *Context) Suggestion {
			n := c.net()
			table := tableOr(c, "")
			if m := reRLSTable.FindStringSubmatch(n.Message); m != nil {
				table = m[1]
			}
			if table == "" {
				table = "the table"
			}
			verb, noun := opOf(n.Method)
			anon := n.Status == 401
			why := fmt.Sprintf("Row-level security (RLS) is switched on for `%s`, but no policy allows %s it for this person.", table, noun)
			who := "the logged-in user"
			if anon {
				why += " The request came from someone who isn't logged in, so Supabase treated them as an anonymous visitor."
				who = "a visitor who isn't logged in"
			}
			prompt := fmt.Sprintf("Supabase rejects %s `%s` with error 42501 \"%s\" (HTTP %d). It happens on %s for %s.\n\nAdd a row-level security policy on `%s` that allows logged-in users to %s only their own rows (for example where the row's `user_id` equals `auth.uid()`). If `%s` has no column linking each row to its user, add `user_id uuid references auth.users default auth.uid()` in the same migration.",
				noun, table, n.Message, n.Status, c.page(), who, table, verb, table)
			if anon {
				prompt += fmt.Sprintf(" If this action should only work for logged-in users, also make %s ask people to log in before they can do it.", c.page())
			}
			prompt += "\n\nDon't turn off row-level security and don't give anonymous users access to the whole table."
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("A database security rule blocks %s %s", noun, table),
				WhatBroke:  fmt.Sprintf("On %s, the app tries to %s data in the `%s` table and Supabase refuses, so the action fails for the person using it.", c.page(), verb, table),
				Why:        why,
				FixPrompt:  prompt,
				HowToCheck: fmt.Sprintf("Log in, go to %s and repeat the action that failed: it should work. Then log out and try again: it should be refused with a clear message, not crash.", c.page()),
				Confidence: 0.75,
			}
		},
	},
	{
		key:   "pg.missing_table",
		match: func(c *Context) bool { return pgCode(c, "PGRST205", "42P01") },
		build: func(c *Context) Suggestion {
			n, table := c.net(), tableOr(c, "the table")
			guess := ""
			if m := reHintTable.FindStringSubmatch(n.Hint); m != nil && m[1] != table {
				guess = m[1]
			}
			why := fmt.Sprintf("The code asks Supabase for a table named `%s`, but the database has no table with that name.", table)
			prompt := fmt.Sprintf("On %s, the app queries the Supabase table `%s`, which doesn't exist (error %s: \"%s\").\n\n", c.page(), table, n.Code, n.Message)
			if guess != "" {
				why += fmt.Sprintf(" Supabase suggests `%s` may be the table that was meant.", guess)
				prompt += fmt.Sprintf("Supabase suggests the existing table `%s`. If that table holds this data, change the code that queries `%s` to use `%s`. Only if `%s` is genuinely a new table the app needs, create it with a migration instead. Don't rename existing tables.", guess, table, guess, table)
			} else {
				prompt += fmt.Sprintf("Check which tables exist in Supabase. If the code uses the wrong name, change it to the correct existing table. If `%s` is a new table the app needs, create it with a migration with the columns the code on %s uses. Don't rename existing tables.", table, c.page())
			}
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("The app reads a table that doesn't exist: %s", table),
				WhatBroke:  fmt.Sprintf("On %s, loading data fails because the app asks for the `%s` table, which isn't in your database.", c.page(), table),
				Why:        why,
				FixPrompt:  prompt,
				HowToCheck: fmt.Sprintf("Open %s and use the part that loads this data. It should load without errors.", c.page()),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "pg.single_row",
		match: func(c *Context) bool { return pgCode(c, "PGRST116") },
		build: func(c *Context) Suggestion {
			table := tableOr(c, "the table")
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("%s expects exactly one %s row but finds none", strings.TrimPrefix(c.page(), "the "), table),
				WhatBroke:  fmt.Sprintf("On %s, loading from `%s` fails when the expected row isn't there (or there are several).", c.page(), table),
				Why:        "The code asks Supabase for exactly one row (`.single()`), so Supabase returns an error when it finds zero or more than one.",
				FixPrompt:  fmt.Sprintf("On %s, the code loads one row from the Supabase table `%s` with `.single()`, and it fails with PGRST116 when no matching row exists.\n\nChange that query to `.maybeSingle()` and handle the case where nothing is found (show an empty state, or create the row if the app expects one to exist). Don't change the table.", c.page(), table),
				HowToCheck: fmt.Sprintf("Open %s with an account that has no `%s` row yet. The page should show an empty state instead of an error.", c.page(), table),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "pg.unique_violation",
		match: func(c *Context) bool { return pgCode(c, "23505") },
		build: func(c *Context) Suggestion {
			n, table := c.net(), tableOr(c, "the table")
			col := "value"
			if m := reDetailKey.FindStringSubmatch(n.Details); m != nil {
				col = m[1]
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("Saving to %s fails because the %s is already taken", table, col),
				WhatBroke:  fmt.Sprintf("On %s, saving fails when the `%s` already exists in `%s`, and the person isn't told why.", c.page(), col, table),
				Why:        fmt.Sprintf("`%s` must be unique in `%s`, and the app tries to insert a duplicate.", col, table),
				FixPrompt:  fmt.Sprintf("On %s, inserting into the Supabase table `%s` fails with error 23505 (duplicate `%s`).\n\nBefore saving, handle the case where a row with the same `%s` already exists: either update that row instead (upsert on `%s`) or show a clear message such as \"This %s is already registered\". Don't remove the unique rule on `%s`; it protects your data.", c.page(), table, col, col, col, col, col),
				HowToCheck: fmt.Sprintf("On %s, save the same %s twice. The second time you should see a clear message (or the existing row is updated), not an error.", c.page(), col),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "pg.not_null",
		match: func(c *Context) bool { return pgCode(c, "23502") },
		build: func(c *Context) Suggestion {
			n, table, col := c.net(), tableOr(c, "the table"), "a required column"
			if m := reNotNull.FindStringSubmatch(n.Message); m != nil {
				col, table = m[1], m[2]
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("Saving to %s fails because %s is missing", table, col),
				WhatBroke:  fmt.Sprintf("On %s, saving fails because the app doesn't send a value for `%s`, which `%s` requires.", c.page(), col, table),
				Why:        fmt.Sprintf("The column `%s` in `%s` can't be empty, but the app saves rows without it.", col, table),
				FixPrompt:  fmt.Sprintf("On %s, inserting into the Supabase table `%s` fails with error 23502 because the required column `%s` is empty.\n\nMake the code on %s send a value for `%s` (and make the form field required if the person types it in). Only if there is a sensible default for every row, give `%s` a default value with a migration instead. Don't make `%s` optional.", c.page(), table, col, c.page(), col, col, col),
				HowToCheck: fmt.Sprintf("On %s, fill in the form and save. It should save, and the new row in `%s` should have `%s` filled in.", c.page(), table, col),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "pg.foreign_key",
		match: func(c *Context) bool { return pgCode(c, "23503") },
		build: func(c *Context) Suggestion {
			n, table := c.net(), tableOr(c, "the table")
			col, ref := "the linked id", "the linked table"
			if m := reDetailKey.FindStringSubmatch(n.Details); m != nil {
				col = m[1]
			}
			if m := reFKTable.FindStringSubmatch(n.Details); m != nil {
				ref = m[1]
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("Saving to %s points to a %s row that doesn't exist", table, ref),
				WhatBroke:  fmt.Sprintf("On %s, saving a row in `%s` fails because its `%s` refers to a row in `%s` that doesn't exist.", c.page(), table, col, ref),
				Why:        fmt.Sprintf("`%s.%s` must match an existing row in `%s`; the app sends an id that is wrong, empty or not created yet.", table, col, ref),
				FixPrompt:  fmt.Sprintf("On %s, inserting into the Supabase table `%s` fails with error 23503: `%s` refers to a row in `%s` that doesn't exist.\n\nFind where the code sets `%s` and make sure it uses the id of a row that really exists in `%s` (create that row first if needed, and wait for its id). Don't remove the foreign key.", c.page(), table, col, ref, col, ref),
				HowToCheck: fmt.Sprintf("Repeat the action on %s. It should save, and the new `%s` row should link to an existing `%s` row.", c.page(), table, ref),
				Confidence: 0.75,
			}
		},
	},
	{
		key:   "pg.invalid_input",
		match: func(c *Context) bool { return pgCode(c, "22P02") },
		build: func(c *Context) Suggestion {
			n, table := c.net(), tableOr(c, "the table")
			typ, val := "the right type", "a bad value"
			if m := reBadInput.FindStringSubmatch(n.Message); m != nil {
				typ, val = m[1], m[2]
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("The app sends %q where %s expects a %s", val, table, typ),
				WhatBroke:  fmt.Sprintf("On %s, a request to `%s` fails because a value isn't a valid %s.", c.page(), table, typ),
				Why:        fmt.Sprintf("The app sends `%s` where Supabase expects a %s, usually an id that hasn't loaded yet.", val, typ),
				FixPrompt:  fmt.Sprintf("On %s, a query to the Supabase table `%s` fails with error 22P02 \"%s\": the app sends `%s` where a %s is expected.\n\nIn the code on %s that queries `%s`, don't run the query until that id is available (check it isn't undefined or empty first), and pass the real id.", c.page(), table, n.Message, val, typ, c.page(), table),
				HowToCheck: fmt.Sprintf("Open %s fresh (reload it). The data should load without errors, including right after the page opens.", c.page()),
				Confidence: 0.75,
			}
		},
	},
	{
		key:   "auth.invalid_credentials",
		match: func(c *Context) bool { return authCode(c, "invalid_credentials", "invalid_grant") },
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity:   "low",
				Title:      "People typed a wrong email or password",
				WhatBroke:  fmt.Sprintf("Log-in attempts on %s were rejected because the email or password was wrong.", c.page()),
				Why:        "This is usually normal user behaviour, not a bug. It matters if many people hit it, or if they can't see what went wrong and give up.",
				FixPrompt:  fmt.Sprintf("On %s, when Supabase login returns \"Invalid login credentials\", show a clear message next to the form (\"Wrong email or password\") and a \"Forgot password?\" link that starts Supabase's password reset (`resetPasswordForEmail`). Don't change how sign-up or login work otherwise.", c.page()),
				HowToCheck: fmt.Sprintf("On %s, log in with a wrong password. You should see the message and the reset link.", c.page()),
				Confidence: 0.7,
			}
		},
	},
	{
		key:   "auth.weak_password",
		match: func(c *Context) bool { return authCode(c, "weak_password") },
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      "Sign-up fails because the password is too weak",
				WhatBroke:  fmt.Sprintf("On %s, sign-up is rejected because the password doesn't meet Supabase's password rules.", c.page()),
				Why:        "Supabase requires a minimum password strength, but the form doesn't tell people the rules before they submit.",
				FixPrompt:  fmt.Sprintf("On %s, Supabase sign-up fails with `weak_password` (\"%s\"). Show the password rules under the password field and check them before submitting (at least the minimum length Supabase requires), and show Supabase's message if it still rejects the password. Don't change the Supabase password settings.", c.page(), c.net().Message),
				HowToCheck: fmt.Sprintf("On %s, try to sign up with the password \"123\". You should see the rules before anything is sent.", c.page()),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "auth.email_not_confirmed",
		match: func(c *Context) bool { return authCode(c, "email_not_confirmed") },
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      "People can't log in because they haven't confirmed their email",
				WhatBroke:  fmt.Sprintf("On %s, log-in fails for people who signed up but never clicked the confirmation email.", c.page()),
				Why:        "Supabase requires email confirmation, and the app doesn't tell people to check their inbox.",
				FixPrompt:  fmt.Sprintf("On %s, Supabase login fails with `email_not_confirmed`. Show \"Please confirm your email first; check your inbox\" with a \"Resend email\" button that calls `supabase.auth.resend({ type: 'signup', email })`. Don't turn off email confirmation.", c.page()),
				HowToCheck: "Sign up with a new email, don't confirm it, and log in. You should see the message and be able to resend the email.",
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "auth.user_exists",
		match: func(c *Context) bool { return authCode(c, "user_already_exists", "email_exists") },
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      "Sign-up fails for people who already have an account",
				WhatBroke:  fmt.Sprintf("On %s, sign-up is rejected because an account with that email already exists.", c.page()),
				Why:        "People who already signed up try again, and the form doesn't point them to log in instead.",
				FixPrompt:  fmt.Sprintf("On %s, Supabase sign-up fails with `%s`. Show \"You already have an account\" with a link to log in and a \"Forgot password?\" link. Don't change the sign-up logic otherwise.", c.page(), c.net().Code),
				HowToCheck: fmt.Sprintf("On %s, sign up with an email that already has an account. You should see the message and links.", c.page()),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "auth.email_rate_limit",
		match: func(c *Context) bool { return authCode(c, "over_email_send_rate_limit") },
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity:   "high",
				Title:      "Supabase stopped sending sign-up and login emails (hourly limit)",
				WhatBroke:  "New sign-ups and password resets fail because Supabase's built-in email sender reached its hourly limit.",
				Why:        "Supabase's default email service is only for testing and allows very few emails per hour. Real apps need their own email provider (custom SMTP) set in the Supabase dashboard (Authentication → Emails → SMTP settings).",
				FixPrompt:  fmt.Sprintf("Supabase auth fails with `over_email_send_rate_limit` on %s. Show people a clear message (\"We couldn't send the email right now, please try again in a few minutes\") instead of failing silently. Don't retry automatically in a loop.", c.page()),
				HowToCheck: "Trigger a sign-up email. When the limit is hit you should see the message; after setting up custom SMTP in Supabase, emails should arrive.",
				Confidence: 0.85,
			}
		},
	},
	{
		key: "net.local_address",
		match: func(c *Context) bool {
			// Only when nothing answered: a local address that responds (a dev
			// proxy, a tunnel) is reachable, and its error says what's wrong.
			n := c.net()
			return n != nil && n.Status == 0 && reLocalHost.MatchString(n.Host)
		},
		build: func(c *Context) Suggestion {
			n := c.net()
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("The app calls a local address (%s) that users can't reach", n.Host),
				WhatBroke:  fmt.Sprintf("On %s, a request to `%s%s` fails for everyone, because that address only exists on a developer's own computer.", c.page(), n.Host, n.Path),
				Why:        "A development URL was left in the code (or an environment variable wasn't set for the published app).",
				FixPrompt:  fmt.Sprintf("On %s, the app calls `%s%s` (%s), which only works on a developer's computer, so it fails for every user (\"%s\").\n\nReplace that address with the real deployed service URL, read from an environment variable, and make the page show a friendly message if the request fails. Don't change what the request does.", c.page(), n.Host, n.Path, n.Method, n.Error),
				HowToCheck: fmt.Sprintf("Open the published app on %s. The browser's network tab should show the request going to the real service and succeeding.", c.page()),
				Confidence: 0.85,
			}
		},
	},
	{
		key: "net.unreachable",
		match: func(c *Context) bool {
			n := c.net()
			return n != nil && n.Status == 0
		},
		build: func(c *Context) Suggestion {
			n := c.net()
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("Requests to %s fail before getting an answer", n.Host),
				WhatBroke:  fmt.Sprintf("On %s, calls to `%s%s` fail with no response (\"%s\").", c.page(), n.Host, n.Path, n.Error),
				Why:        "The browser couldn't reach the service: the URL may be wrong, the service down, or the browser blocking it because the service doesn't allow requests from your app's domain (CORS).",
				FixPrompt:  fmt.Sprintf("On %s, the request %s `https://%s%s` fails in the browser with \"%s\" (no response at all).\n\nCheck that this URL is correct and that the service allows requests from the app's domain (CORS). Fix whichever is wrong, and make the page show a friendly message instead of breaking when the request fails.", c.page(), n.Method, n.Host, n.Path, n.Error),
				HowToCheck: fmt.Sprintf("Open %s with the browser's network tab open. The request to %s should succeed.", c.page(), n.Host),
				Confidence: 0.55,
			}
		},
	},
	{
		key: "js.null_property",
		match: func(c *Context) bool {
			j := c.js()
			return j != nil && j.Type == "TypeError" && j.Property != "" &&
				(strings.Contains(j.Message, "of undefined") || strings.Contains(j.Message, "of null") ||
					strings.Contains(j.Message, "is undefined") || strings.Contains(j.Message, "is null") ||
					strings.Contains(j.Message, "not an object"))
		},
		build: func(c *Context) Suggestion {
			j := c.js()
			near := ""
			if j.Function != "" {
				near = fmt.Sprintf(" (the crash is near code named `%s`)", j.Function)
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("%s crashes when `%s` isn't there yet", capFirst(c.page()), j.Property),
				WhatBroke:  fmt.Sprintf("On %s, the page crashes with \"%s\" and people can't continue.", c.page(), j.Message),
				Why:        fmt.Sprintf("The code reads `%s` from something that is empty at that moment, usually data that hasn't finished loading, a record that doesn't exist, or a logged-out user.", j.Property),
				FixPrompt:  fmt.Sprintf("On %s, the code reads `.%s` from a value that is sometimes undefined or null%s, which crashes with \"%s: %s\".\n\nFind where `.%s` is read on %s and handle the empty case: wait until the data has loaded (show a loading state), and show an empty state or a message if it never arrives. Keep the change to that spot; don't rewrite the component.", c.page(), j.Property, near, j.Type, j.Message, j.Property, c.page()),
				HowToCheck: fmt.Sprintf("Open %s on a slow connection, logged out, and with an account that has no data. It should never crash; you should see a loading or empty state.", c.page()),
				Confidence: 0.65,
			}
		},
	},
	{
		key: "js.not_a_function",
		match: func(c *Context) bool {
			j := c.js()
			return j != nil && j.Type == "TypeError" && reNotFunction.MatchString(j.Message)
		},
		build: func(c *Context) Suggestion {
			j := c.js()
			ident := sourceIdent(reNotFunction.FindStringSubmatch(j.Message)[1])
			listy := regexp.MustCompile(`\.(map|filter|forEach|reduce|find|some|every|slice|sort|join|includes)$`).MatchString(ident)
			why := fmt.Sprintf("The code calls `%s(...)`, but at that moment it isn't a function.", ident)
			fix := fmt.Sprintf("On %s, the app crashes with \"%s: %s\". Find where `%s(...)` is called on %s and make sure the value is what the code expects before calling it.", c.page(), j.Type, j.Message, ident, c.page())
			if listy {
				why = fmt.Sprintf("The code uses `%s` on data that isn't a list (for example an object, null or an error response).", ident)
				fix = fmt.Sprintf("On %s, the app crashes with \"%s: %s\": the data isn't a list when the code loops over it.\n\nFind where `%s` is used on %s. Make sure the data is a list before using it (default to an empty list `[]` while loading, and check the API returns a list, not an object or an error). Keep the change to that spot.", c.page(), j.Type, j.Message, ident, c.page())
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("%s crashes: %s is not a function", capFirst(c.page()), ident),
				WhatBroke:  fmt.Sprintf("On %s, the page crashes with \"%s\".", c.page(), j.Message),
				Why:        why,
				FixPrompt:  fix,
				HowToCheck: fmt.Sprintf("Open %s, including with an empty account. It should show the list (or an empty state) without crashing.", c.page()),
				Confidence: 0.65,
			}
		},
	},
	{
		key: "js.not_defined",
		match: func(c *Context) bool {
			j := c.js()
			return j != nil && j.Type == "ReferenceError" && reNotDefined.MatchString(j.Message)
		},
		build: func(c *Context) Suggestion {
			j := c.js()
			name := reNotDefined.FindStringSubmatch(j.Message)[1]
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("%s crashes: %s is missing", capFirst(c.page()), name),
				WhatBroke:  fmt.Sprintf("On %s, the page crashes because `%s` doesn't exist in the code.", c.page(), name),
				Why:        fmt.Sprintf("The code uses `%s`, but it was never imported or defined (often after a rename or a removed file).", name),
				FixPrompt:  fmt.Sprintf("On %s, the app crashes with \"ReferenceError: %s\". Find where `%s` is used and add the missing import or definition (check whether it was renamed or moved). Don't change anything else.", c.page(), j.Message, name),
				HowToCheck: fmt.Sprintf("Open %s and use it normally. It should not crash.", c.page()),
				Confidence: 0.75,
			}
		},
	},
	{
		key: "js.chunk_load",
		match: func(c *Context) bool {
			j := c.js()
			return j != nil && (strings.Contains(j.Message, "dynamically imported module") ||
				strings.Contains(j.Message, "Loading chunk") || j.Type == "ChunkLoadError" ||
				strings.Contains(j.Message, "Importing a module script failed"))
		},
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      "Pages fail to open for people who had the app open during an update",
				WhatBroke:  "After you publish a new version, people who still have the old version open get an error when they move to another page.",
				Why:        "Their browser asks for a file from the old version, which no longer exists after the update.",
				FixPrompt:  "When a page's code fails to load (\"Failed to fetch dynamically imported module\" or a chunk load error), reload the page once automatically so the person gets the new version. Catch this error where pages are lazy-loaded (or in the router's error handler), call `window.location.reload()`, and use `sessionStorage` to make sure it only reloads once, so it can never loop. Don't change anything else.",
				HowToCheck: "Open the app, publish a new version, then click to another page in the old tab. It should reload once and work.",
				Confidence: 0.8,
			}
		},
	},
	{
		key: "js.json_parse",
		match: func(c *Context) bool {
			j := c.js()
			return j != nil && j.Type == "SyntaxError" && (strings.Contains(j.Message, "JSON") || strings.Contains(j.Message, "Unexpected token"))
		},
		build: func(c *Context) Suggestion {
			j := c.js()
			if j.JSONPath != "" {
				typ := j.JSONType
				if i := strings.IndexByte(typ, ';'); i >= 0 {
					typ = typ[:i]
				}
				if typ == "" {
					typ = "no content type"
				}
				return Suggestion{
					Severity:   c.severity("medium"),
					Title:      fmt.Sprintf("%s expects JSON from %s but gets %s", capFirst(c.page()), j.JSONPath, typ),
					WhatBroke:  fmt.Sprintf("On %s, the request to `%s` returns %s (HTTP %d) instead of JSON, so reading it crashes with \"%s\".", c.page(), j.JSONPath, typ, j.JSONStatus, j.Message),
					Why:        fmt.Sprintf("`%s` isn't a JSON endpoint: usually the URL is wrong and the app's own page (HTML) comes back instead, or the server returned an error page.", j.JSONPath),
					FixPrompt:  fmt.Sprintf("On %s, the app fetches `%s` and calls `.json()` on the response, but the response is %s (HTTP %d), not JSON, so it crashes with \"%s: %s\".\n\nFind that request. Check that `%s` is the right URL (fix it if it isn't), and before calling `.json()` check `response.ok` and that the content type is JSON; if not, show a friendly error instead of crashing. Don't change anything else.", c.page(), j.JSONPath, typ, j.JSONStatus, j.Type, j.Message, j.JSONPath),
					HowToCheck: fmt.Sprintf("Open %s with the network tab open. The request to %s should return JSON, and if it fails the page should show a message, not crash.", c.page(), j.JSONPath),
					Confidence: 0.75,
				}
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("%s fails reading a response that isn't JSON", capFirst(c.page())),
				WhatBroke:  fmt.Sprintf("On %s, the app crashes with \"%s\" while reading a server response.", c.page(), j.Message),
				Why:        "The app expects JSON but gets something else, usually an HTML error page because the request URL is wrong or the server failed.",
				FixPrompt:  fmt.Sprintf("On %s, the app crashes with \"%s: %s\" when it calls `.json()` on a response that isn't JSON.\n\nFind that request. Check the URL is correct, and before calling `.json()` check `response.ok` (and that the content type is JSON); if not, show a friendly error instead of crashing. Don't change anything else.", c.page(), j.Type, j.Message),
				HowToCheck: fmt.Sprintf("Open %s with the network tab open. The request should return JSON, and if it fails the page should show a message, not crash.", c.page()),
				Confidence: 0.7,
			}
		},
	},
}

// sourceIdent drops a minified variable name at the start of a property path:
// "hg.items.map" → "items.map". Property names survive minification and can
// be searched for in the source; minified variable names can't.
func sourceIdent(path string) string {
	parts := strings.Split(path, ".")
	if len(parts) > 1 && len(parts[0]) <= 2 {
		return strings.Join(parts[1:], ".")
	}
	return path
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
