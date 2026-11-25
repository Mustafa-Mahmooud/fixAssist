package ai

import (
	"fmt"
	"regexp"
	"strings"
)

// The extended catalog: the failures that most often break apps built with
// AI builders and their backends (Supabase, Firebase, React, Stripe, OpenAI,
// email) and servers in any language. Each entry recognizes the wording of
// the real error, quotes the exact line it matched, and asks for one
// specific change.
//
// Order matters only where two entries could match the same error, so the
// full catalog is assembled here, specific before general:
//
//	catalogFirst  noise to ignore, Supabase/Firebase setup, third-party APIs
//	baseCatalog   the original Phase 4 entries (catalog.go)
//	browserMore   more PostgREST, Storage, Auth, Firebase, React and browser errors
//	serverFirst   server errors that must win over serverCatalog (e.g. SMTP before "connection refused")
//	serverCatalog the server entries of server.go
//	catalogLast   broad network and timeout entries
var catalog []entry

func init() {
	for _, group := range [][]entry{catalogFirst, baseCatalog, browserMore, serverFirst, serverCatalog, catalogLast} {
		catalog = append(catalog, group...)
	}
}

// text is one message about the issue: what is matched (for a failed
// request: code, message, browser error and body) and what is quoted (the
// message alone).
type text struct{ match, quote string }

// sources is every message we have about the issue, across the browser
// (page errors, failed requests) and the server.
func (c *Context) sources() []text {
	var out []text
	add := func(match, quote string) { out = append(out, text{match, quote}) }
	for _, j := range c.JS {
		t := strings.TrimPrefix(j.Type+": "+j.Message, ": ")
		add(t, t)
	}
	for i := range c.Net {
		n := &c.Net[i]
		add(strings.TrimSpace(strings.Join([]string{n.Code, n.Message, n.Error, n.Body}, " ")), netLine(n))
	}
	for _, s := range c.Server {
		for _, x := range s.Chain {
			add(x.Type+": "+x.Message, x.Type+": "+x.Message)
		}
	}
	if c.Message != "" {
		add(c.Message, c.Message)
	}
	return out
}

// texts is what is matched against.
func (c *Context) texts() []string {
	var out []string
	for _, t := range c.sources() {
		out = append(out, t.match)
	}
	return out
}

// found is the quote of the first message matching re.
func (c *Context) found(re *regexp.Regexp) string {
	for _, t := range c.sources() {
		if re.MatchString(t.match) {
			return truncate(strings.Join(strings.Fields(t.quote), " "), 300)
		}
	}
	return ""
}

func textMatch(re *regexp.Regexp) func(c *Context) bool {
	return func(c *Context) bool { return c.found(re) != "" }
}

// netLine is a failed request's own words.
func netLine(n *NetSample) string {
	for _, s := range []string{n.Message, n.Error, n.Body} {
		if s != "" {
			return truncate(strings.Join(strings.Fields(s), " "), 300)
		}
	}
	return ""
}

// opening states where and how it failed, in the error's own words.
func opening(c *Context, line string) string {
	if s := c.srv(); s != nil {
		return fmt.Sprintf("My app's %s fails on %s with \"%s\".", serverName(s.Platform), c.page(), line)
	}
	if n := c.net(); n != nil {
		if line == "" {
			line = netLine(n)
		}
		said := ""
		if line != "" {
			said = fmt.Sprintf(" (\"%s\")", line)
		}
		if n.Status == 0 {
			return fmt.Sprintf("On %s, the request %s `%s%s` gets no response%s.", c.page(), n.Method, n.Host, n.Path, said)
		}
		return fmt.Sprintf("On %s, the request %s `%s%s` fails with HTTP %d%s.", c.page(), n.Method, n.Host, n.Path, n.Status, said)
	}
	if line == "" {
		return fmt.Sprintf("On %s, the app fails.", c.page())
	}
	return fmt.Sprintf("On %s, the app fails with \"%s\".", c.page(), line)
}

// spec is an entry's text; build fills in the opening and severity.
type spec struct {
	sev, title, what, why, fix, check string
	conf                              float64
}

// mk is an entry that matches re anywhere in the issue's messages (and
// extra, when given), and quotes the matching line.
func mk(key string, re *regexp.Regexp, extra func(c *Context) bool, b func(c *Context, line string) spec) entry {
	return entry{
		key: key,
		match: func(c *Context) bool {
			return (re == nil || textMatch(re)(c)) && (extra == nil || extra(c))
		},
		build: func(c *Context) Suggestion {
			line := ""
			if re != nil {
				line = c.found(re)
			}
			if line == "" {
				if n := c.net(); n != nil {
					line = netLine(n)
				} else if j := c.js(); j != nil {
					line = strings.TrimPrefix(j.Type+": "+j.Message, ": ")
				}
			}
			s := b(c, line)
			return Suggestion{
				Severity: c.severity(s.sev), Title: s.title, WhatBroke: s.what, Why: s.why,
				FixPrompt: opening(c, line) + "\n\n" + s.fix, HowToCheck: s.check, Confidence: s.conf,
			}
		},
	}
}

func isServer(c *Context) bool  { return c.srv() != nil }
func isBrowser(c *Context) bool { return c.srv() == nil }

// netWhere reports whether the issue is a failed request whose path or host matches.
func netWhere(pathPrefix string, statuses ...int) func(c *Context) bool {
	return func(c *Context) bool {
		n := c.net()
		if n == nil || !strings.HasPrefix(n.Path, pathPrefix) {
			return false
		}
		if len(statuses) == 0 {
			return true
		}
		for _, s := range statuses {
			if n.Status == s {
				return true
			}
		}
		return false
	}
}

func status(codes ...int) func(c *Context) bool {
	return func(c *Context) bool {
		n := c.net()
		if n == nil {
			return false
		}
		for _, s := range codes {
			if n.Status == s {
				return true
			}
		}
		return false
	}
}

// segment is the n-th part of a path: segment("/functions/v1/send-email", 2) = "send-email".
func segment(path string, i int) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if i < len(parts) && !strings.HasPrefix(parts[i], "<") {
		return parts[i]
	}
	return ""
}

// inBrowserKeyWarning is added when a browser calls a paid API directly.
func inBrowserKeyWarning(c *Context) string {
	if isBrowser(c) && c.net() != nil {
		return " Also: the browser is calling this API directly, which means the secret API key is in the website's code and anyone can copy it. Move the call into a server function (a Supabase Edge Function or your backend) that keeps the key in a server-side secret, and rotate the exposed key."
	}
	return ""
}

func whereNamed(c *Context) string {
	if isServer(c) {
		return "server"
	}
	return "app"
}

var (
	reAbort          = regexp.MustCompile(`(?i)AbortError|The user aborted a request|signal is aborted without reason|The operation was aborted|aborted a request`)
	reResizeObserver = regexp.MustCompile(`ResizeObserver loop (?:limit exceeded|completed with undelivered notifications)`)
	reScriptError    = regexp.MustCompile(`^(?:Error: )?Script error\.?$`)
	reSupabaseEnv    = regexp.MustCompile(`(?i)supabaseUrl is required|supabaseKey is required|Invalid supabaseUrl|Invalid URL.*supabase|supabase.*Invalid URL`)
	reFirebaseEnv    = regexp.MustCompile(`(?i)auth/invalid-api-key|No Firebase App '\[DEFAULT\]' has been created|"projectId" not provided in firebase\.initializeApp|Firebase: Need to provide options|auth/configuration-not-found`)
	reEdgeFetch      = regexp.MustCompile(`(?i)Failed to send a request to the Edge Function|FunctionsFetchError`)
	reEdgeHTTP       = regexp.MustCompile(`(?i)Edge Function returned a non-2xx status code|FunctionsHttpError|FunctionsRelayError`)
	reAIKey          = regexp.MustCompile(`(?i)Incorrect API key provided|invalid_api_key|invalid x-api-key|authentication_error.*api.?key|API key not valid|API_KEY_INVALID|Invalid API Key provided|No API key provided|You did not provide an API key`)
	reAIQuota        = regexp.MustCompile(`(?i)You exceeded your current quota|insufficient_quota|credit balance is too low|billing.*(?:hard )?limit|RESOURCE_EXHAUSTED.*quota`)
	reStripeMode     = regexp.MustCompile(`(?i)No such (?:price|customer|product|payment_intent|paymentintent|subscription|plan|coupon|checkout\.session|invoice|payment_method|setupintent): ?'?[\w-]*'?|a similar object exists in (?:test|live) mode`)
	reStripeWebhook  = regexp.MustCompile(`(?i)No signatures found matching the expected signature for payload|Webhook signature verification failed|StripeSignatureVerificationError|SignatureVerificationError|Unable to extract timestamp and signatures from header`)
)

var catalogFirst = []entry{
	// Noise: errors that look alarming but usually aren't bugs.
	mk("auth.refresh_token", reRefreshToken, nil, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.75,
			title: "Returning visitors get an error because their saved session is no longer valid",
			what:  fmt.Sprintf("On %s, the app tries to restore an old login (\"%s\") and fails instead of asking the person to sign in again.", c.page(), line),
			why:   "The saved session was signed out elsewhere, expired, or refreshed by another tab or a second Supabase client at the same moment.",
			fix:   "Make sure the app creates exactly one Supabase client (imported everywhere from one file). When restoring the session fails with this error, call `supabase.auth.signOut({ scope: 'local' })` to clear the stale session and send the person to the sign-in page, instead of showing an error.",
			check: "Sign in, then sign out from another browser (or delete the session in Supabase), and reload the app. It should show the sign-in page, not an error."}
	}),
	mk("noise.aborted", reAbort, isBrowser, func(c *Context, line string) spec {
		return spec{sev: "low", conf: 0.7,
			title: "Requests cancelled on purpose are reported as errors",
			what:  fmt.Sprintf("On %s, a request was cancelled before it finished (\"%s\"). This usually happens when someone leaves the page or a component reloads its data.", c.page(), line),
			why:   "Cancelling a request throws an AbortError, and the code treats it like a real failure.",
			fix:   "Find where this request is made with an AbortController (or where the component cancels its fetch on unmount). In its error handling, ignore errors whose name is `AbortError` instead of showing or reporting them. Don't remove the cancellation itself: it prevents updating a page that is gone.",
			check: fmt.Sprintf("Open %s and leave it quickly, several times. No error should be reported and nothing should break.", c.page())}
	}),
	mk("noise.resize_observer", reResizeObserver, nil, func(c *Context, line string) spec {
		return spec{sev: "low", conf: 0.8,
			title: "Harmless browser warning about layout resizing",
			what:  fmt.Sprintf("On %s, the browser reports \"%s\". Visitors don't see anything wrong.", c.page(), line),
			why:   "A component changed its size while the browser was measuring layout. The browser reports it, but nothing breaks.",
			fix:   "This is usually harmless. If it comes from a component that measures itself (charts, text areas that grow, virtual lists), wrap the size update in `requestAnimationFrame`. Otherwise, filter this exact message out of error reporting. Don't change layout code just to silence it.",
			check: fmt.Sprintf("Open %s and resize the window. Nothing should look broken.", c.page())}
	}),
	mk("noise.script_error", nil, func(c *Context) bool {
		j := c.js()
		return j != nil && reScriptError.MatchString(strings.TrimSpace(strings.TrimPrefix(j.Type+": "+j.Message, ": ")))
	}, func(c *Context, line string) spec {
		return spec{sev: "low", conf: 0.6,
			title: "An error in a script from another website, with its details hidden",
			what:  fmt.Sprintf("On %s, a script loaded from another domain (a widget, analytics, a CDN library) threw an error, and the browser hid the details.", c.page()),
			why:   "Browsers hide error details from scripts on other domains unless the script is loaded with `crossorigin` and served with CORS headers.",
			fix:   "Find the `<script>` tags that load code from other domains. Add `crossorigin=\"anonymous\"` to those whose server supports CORS (most CDNs do), so the next occurrence shows the real error. Don't remove any script.",
			check: "After publishing, if the error happens again it should appear with a real message instead of \"Script error.\"."}
	}),

	// Supabase and Firebase setup.
	mk("sb.env_missing", reSupabaseEnv, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.85,
			title: "The app can't connect to Supabase: its URL or key is missing",
			what:  fmt.Sprintf("On %s, the app stops with \"%s\" because the Supabase connection settings are empty in the published version.", c.page(), line),
			why:   "The Supabase client is created from environment variables that are set on the developer's machine but not in the hosting where the app is published.",
			fix:   "Find where the Supabase client is created (`createClient(...)`) and which variables it reads (for example `VITE_SUPABASE_URL` and `VITE_SUPABASE_ANON_KEY`). Make sure those exact names are set in the hosting's environment settings for production, then rebuild and publish. If the app was built with Lovable or Bolt, check that the Supabase integration is connected for the published app. Only use the anon (public) key in the browser, never the service-role key.",
			check: "Open the published app in a private window. It should load data instead of failing on start."}
	}),
	mk("sb.bad_key", regexp.MustCompile(`(?i)Invalid API key|No API key found in request`), func(c *Context) bool {
		n := c.net()
		return n != nil && strings.Contains(n.Host, "supabase") && (n.Status == 401 || n.Status == 403)
	}, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.85,
			title: "Supabase rejects the app's API key",
			what:  fmt.Sprintf("On %s, every request to Supabase is refused (\"%s\"), so nothing loads or saves.", c.page(), line),
			why:   "The anon key in the published app is wrong: from another Supabase project, mistyped, or rotated in the dashboard without updating the app.",
			fix:   "Copy the project's current anon (public) key from Supabase (Project Settings > API) and set it in the production environment variable the client reads (for example `VITE_SUPABASE_ANON_KEY`), together with the matching project URL. Rebuild and publish. Never use the service-role key in the browser.",
			check: "Open the published app in a private window. Data should load."}
	}),
	mk("fb.env_missing", reFirebaseEnv, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.8,
			title: "The app can't connect to Firebase: its configuration is missing or wrong",
			what:  fmt.Sprintf("On %s, Firebase stops with \"%s\".", c.page(), line),
			why:   "The Firebase config (apiKey, authDomain, projectId, ...) is empty or wrong in the published version, usually because environment variables aren't set in production, or Firebase is used before `initializeApp` runs.",
			fix:   "Find `initializeApp(...)` and the config it gets. Make sure every value is set in the production environment (the same names as in development), that `initializeApp` runs once before any other Firebase call, and that the API key belongs to this Firebase project. For auth/configuration-not-found, enable the sign-in method in the Firebase console (Authentication > Sign-in method).",
			check: "Open the published app in a private window and sign in. Firebase should work."}
	}),
	mk("sb.edge_cors", nil, func(c *Context) bool {
		if c.found(reEdgeFetch) != "" {
			return true
		}
		return netWhere("/functions/v1/", 0)(c)
	}, func(c *Context, line string) spec {
		fn := ""
		if n := c.net(); n != nil {
			fn = segment(n.Path, 2)
		}
		name := orWord(fn, "the Edge Function")
		return spec{sev: "high", conf: 0.75,
			title: fmt.Sprintf("The browser can't call %s", name),
			what:  fmt.Sprintf("On %s, calling %s fails before it gets an answer, so that feature doesn't work.", c.page(), name),
			why:   "Usually the function doesn't answer the browser's CORS preflight (an OPTIONS request) with the right headers, or it isn't deployed.",
			fix:   fmt.Sprintf("In the Supabase Edge Function %s: answer `OPTIONS` requests first with status 200 and CORS headers (`Access-Control-Allow-Origin: *` and `Access-Control-Allow-Headers: authorization, x-client-info, apikey, content-type`), and add the same headers to every other response, including errors. Then make sure the function is deployed (`supabase functions deploy %s`). Don't change the function's logic.", name, map[bool]string{true: fn, false: "<name>"}[fn != ""]),
			check: fmt.Sprintf("Open %s and use the feature with the browser's network tab open. The OPTIONS request and the call to %s should both succeed.", c.page(), name)}
	}),
	mk("sb.edge_missing", nil, netWhere("/functions/v1/", 404), func(c *Context, line string) spec {
		fn := segment(c.net().Path, 2)
		return spec{sev: "high", conf: 0.8,
			title: fmt.Sprintf("The Edge Function %s doesn't exist in production", orWord(fn, "being called")),
			what:  fmt.Sprintf("On %s, the app calls the Edge Function %s, and Supabase answers that it isn't there.", c.page(), orWord(fn, "named in the request")),
			why:   "The function was never deployed to this Supabase project, was deployed under another name, or the app points at another project.",
			fix:   fmt.Sprintf("Check that `supabase/functions/%s` exists in the code and deploy it to the production project (`supabase functions deploy %s`). If the name in the app's `supabase.functions.invoke(...)` call differs from the folder name, make them match.", map[bool]string{true: fn, false: "<name>"}[fn != ""], map[bool]string{true: fn, false: "<name>"}[fn != ""]),
			check: fmt.Sprintf("Use the feature on %s again. The call should return 200.", c.page())}
	}),
	mk("sb.edge_error", nil, func(c *Context) bool {
		return c.found(reEdgeHTTP) != "" || netWhere("/functions/v1/", 400, 401, 403, 422, 500, 502, 503, 504, 546)(c)
	}, func(c *Context, line string) spec {
		fn := ""
		if n := c.net(); n != nil {
			fn = segment(n.Path, 2)
		}
		name := orWord(fn, "an Edge Function")
		return spec{sev: "high", conf: 0.6,
			title: fmt.Sprintf("%s returns an error", capFirst(strings.Trim(name, "`"))),
			what:  fmt.Sprintf("On %s, %s answers with an error, so that feature fails.", c.page(), name),
			why:   "The function itself fails: a missing secret (API key) in the function's environment, unexpected input from the app, or an error from a service it calls.",
			fix:   fmt.Sprintf("Open the logs of %s in the Supabase dashboard (Edge Functions > Logs) for the time of these failures and find the error. The most common causes: a secret the function reads with `Deno.env.get(...)` isn't set (`supabase secrets set NAME=value`), or the request body doesn't match what the function expects. Fix that cause. Also make the function return a clear JSON error with CORS headers instead of crashing, and make the app show a friendly message when it fails.", name),
			check: fmt.Sprintf("Use the feature on %s again. The function should return 200, and its logs should show no error.", c.page())}
	}),

	// Third-party APIs (browser or server).
	mk("api.key_invalid", reAIKey, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.8,
			title: "An API key is wrong, missing or revoked",
			what:  fmt.Sprintf("On %s, a service the %s uses (AI, payments or similar) rejects the request because its API key isn't valid: \"%s\".", c.page(), whereNamed(c), line),
			why:   "The key in production is missing, mistyped, expired or revoked, or a test key is used against live mode.",
			fix:   "Find where this service's client is created and which environment variable or secret provides the key. Set a valid key in the production environment (hosting settings, Supabase secrets or your server's config) under exactly that name, and redeploy. Never hard-code the key." + inBrowserKeyWarning(c),
			check: fmt.Sprintf("Use the feature on %s again. The service should answer normally.", c.page())}
	}),
	mk("api.quota", reAIQuota, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.85,
			title: "A paid API stopped working: the account is out of credit or over its quota",
			what:  fmt.Sprintf("On %s, the AI or API provider refuses requests because the account ran out of credit or hit its limit: \"%s\".", c.page(), line),
			why:   "This is a billing limit on the provider's side, not a code bug.",
			fix:   "This needs the account owner: add credit or raise the usage limit in the provider's billing settings. In the code, handle this error by showing \"This feature is temporarily unavailable\" instead of failing, and don't retry it in a loop." + inBrowserKeyWarning(c),
			check: fmt.Sprintf("After the billing change, use the feature on %s again. It should work, and if the quota runs out again the page should show a clear message.", c.page())}
	}),
	mk("api.stripe_mode", reStripeMode, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.8,
			title: "Payments fail: an ID from Stripe's test mode is used with live keys (or the other way round)",
			what:  fmt.Sprintf("On %s, Stripe can't find an object the app asks for: \"%s\".", c.page(), line),
			why:   "Prices, products and customers created in test mode don't exist in live mode. The app uses IDs from one mode with keys from the other, or from another Stripe account.",
			fix:   "Find where this Stripe ID comes from (usually a hard-coded `price_...` or an environment variable). Use the live-mode ID together with the live secret key in production, and test-mode IDs with test keys in development. Put the IDs in environment variables next to the keys so they always match.",
			check: "Make a real (or 100% discounted) purchase on the live site. Stripe should accept it."}
	}),
	mk("api.stripe_webhook", reStripeWebhook, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.85,
			title: "Stripe webhooks are rejected, so payments may not be recorded",
			what:  fmt.Sprintf("When Stripe notifies the app about a payment, checking the notification's signature fails: \"%s\". Orders or subscriptions may not be updated.", line),
			why:   "The webhook signing secret doesn't match the endpoint (each endpoint, and test vs live, has its own `whsec_...`), or the request body was parsed as JSON before the signature check, which needs the raw body.",
			fix:   "In the webhook handler: verify the signature on the raw request body (for example `express.raw({type: 'application/json'})` on this route only, `await req.text()` in Edge Functions or Next.js route handlers, `Request.Body` read as a string in .NET), and use the signing secret of this exact endpoint from Stripe's dashboard, stored in an environment variable. Don't skip the signature check.",
			check: "In Stripe's dashboard, resend a recent event to this endpoint. It should be answered with 200."}
	}),
}

var (
	reColumnCache   = regexp.MustCompile(`Could not find the '([^']+)' column of '([^']+)'`)
	reFunctionCache = regexp.MustCompile(`Could not find the function (?:public\.)?([\w.]+)\(([^)]*)\)`)
	reRelationship  = regexp.MustCompile(`between '([^']+)' and '([^']+)'`)
	reCheckName     = regexp.MustCompile(`check constraint "([^"]+)"`)
	reJWTExpired    = regexp.MustCompile(`(?i)JWT expired|jwt expired|token is expired|PGRST30[123]`)
	reRefreshToken  = regexp.MustCompile(`(?i)refresh_token_not_found|refresh_token_already_used|Invalid Refresh Token|session_not_found|Refresh Token Not Found`)
	reOTPExpired    = regexp.MustCompile(`(?i)otp_expired|Email link is invalid or has expired|flow_state_expired|flow_state_not_found|bad_code_verifier|invalid flow state|Token has expired or is invalid`)
	reFBPermission  = regexp.MustCompile(`(?i)Missing or insufficient permissions|permission-denied|PERMISSION_DENIED`)
	reFBIndex       = regexp.MustCompile(`(?i)The query requires an index|requires a COLLECTION_ASC index|FAILED_PRECONDITION.*index`)
	reFBIndexLink   = regexp.MustCompile(`https://console\.firebase\.google\.com/\S+`)
	reFBCredential  = regexp.MustCompile(`auth/(?:invalid-credential|wrong-password|user-not-found|invalid-login-credentials|invalid-email)|INVALID_LOGIN_CREDENTIALS|EMAIL_NOT_FOUND|INVALID_PASSWORD`)
	reFBEmailInUse  = regexp.MustCompile(`auth/email-already-in-use|EMAIL_EXISTS`)
	reFBWeak        = regexp.MustCompile(`auth/weak-password|WEAK_PASSWORD`)
	reFBDomain      = regexp.MustCompile(`auth/unauthorized-domain|auth/unauthorized-continue-uri`)
	reFBPopup       = regexp.MustCompile(`auth/popup-closed-by-user|auth/cancelled-popup-request|auth/popup-blocked|auth/user-cancelled`)
	reHydration     = regexp.MustCompile(`(?i)Hydration failed|hydrating|Text content does not match server-rendered HTML|did not match\. Server:|Minified React error #(?:418|423|425)\b`)
	reRenderLoop    = regexp.MustCompile(`(?i)Too many re-renders|Maximum update depth exceeded|Minified React error #(?:185|301)\b`)
	reHooks         = regexp.MustCompile(`(?i)Rendered (?:more|fewer) hooks than|Invalid hook call|change in the order of Hooks|Minified React error #(?:300|310|321)\b`)
	reNoProvider    = regexp.MustCompile(`(?i)(use\w+)\(\) may be used only in the context of a <(\w+)> component|(use\w+) must be used within (?:an? )?(\w+)|could not find react-redux context value|No QueryClient set, use QueryClientProvider|useNavigate\(\) may be used only`)
	reObjectChild   = regexp.MustCompile(`(?i)Objects are not valid as a React child|Minified React error #31\b`)
	reInvalidDate   = regexp.MustCompile(`(?i)Invalid time value|Invalid Date|date value out of bounds`)
	reStackOverflow = regexp.MustCompile(`(?i)Maximum call stack size exceeded|too much recursion|StackOverflowError|RecursionError: maximum recursion depth|stack overflow`)
	reQuota         = regexp.MustCompile(`(?i)QuotaExceededError|exceeded the quota|quota has been exceeded|Setting the value of '[^']+' exceeded`)
)

var browserMore = []entry{
	// PostgREST (Supabase database API).
	{
		key: "pg.column_missing",
		match: func(c *Context) bool {
			return c.net() != nil && (pgCode(c, "PGRST204") || c.found(reColumnCache) != "")
		},
		build: func(c *Context) Suggestion {
			line := c.found(reColumnCache)
			col, table := "", tableOr(c, "")
			if m := reColumnCache.FindStringSubmatch(line); m != nil {
				col, table = m[1], m[2]
			}
			verb, _ := opOf(c.net().Method)
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("Saving fails: the database has no column %s in %s", orWord(col, "named in the error"), orWord(table, "the table")),
				WhatBroke:  fmt.Sprintf("On %s, the app tries to %s data with a field %s that the %s table doesn't have, so nothing is saved.", c.page(), verb, orWord(col, "named in the error"), orWord(table, "database")),
				Why:        "The code and the database disagree: the column was renamed or never created in the live database, or the app sends a field (often a camelCase name like createdAt) that isn't a column.",
				FixPrompt:  opening(c, line) + fmt.Sprintf("\n\nEither add the column %s to the table %s with a new migration (if the app really needs to store it), or fix the code so it sends the existing column's exact name and drops fields that aren't columns. Check the table's columns first and pick the right one. Don't change other tables.", orWord(col, "named in the error"), orWord(table, "named in the error")),
				HowToCheck: fmt.Sprintf("On %s, submit the form again. It should save.", c.page()),
				Confidence: 0.85,
			}
		},
	},
	{
		key: "pg.function_missing",
		match: func(c *Context) bool {
			return c.net() != nil && (pgCode(c, "PGRST202") || c.found(reFunctionCache) != "")
		},
		build: func(c *Context) Suggestion {
			line := c.found(reFunctionCache)
			fn, args := RPCFromPath(c.net().Path), ""
			if m := reFunctionCache.FindStringSubmatch(line); m != nil {
				fn, args = m[1], m[2]
			}
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("The database function %s can't be found", orWord(fn, "being called")),
				WhatBroke:  fmt.Sprintf("On %s, the app calls the database function %s, and the database can't find one with those parameters.", c.page(), orWord(fn, "named in the request")),
				Why:        "The function wasn't created in the live database, or its parameter names or types differ from what the app sends (PostgREST matches functions by parameter names).",
				FixPrompt:  opening(c, line) + fmt.Sprintf("\n\nCompare the app's `supabase.rpc('%s', {...})` call with the function's definition in the migrations. Make the argument names in the call match the function's parameter names exactly (the app sends: %s), or create the missing function with a migration. After changing a function, reload the schema cache (`notify pgrst, 'reload schema';`).", map[bool]string{true: fn, false: "<name>"}[fn != ""], map[bool]string{true: "`" + args + "`", false: "see the error"}[args != ""]),
				HowToCheck: fmt.Sprintf("Use the feature on %s again. The call should return 200.", c.page()),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "pg.relationship_missing",
		match: func(c *Context) bool { return pgCode(c, "PGRST200", "PGRST201") },
		build: func(c *Context) Suggestion {
			n := c.net()
			a, b := "", ""
			if m := reRelationship.FindStringSubmatch(n.Message); m != nil {
				a, b = m[1], m[2]
			}
			ambiguous := n.Code == "PGRST201"
			why := "The query embeds one table inside another (`select('*, other(*)')`), but there is no foreign key between them in the database, so Supabase doesn't know how they're linked."
			fix := fmt.Sprintf("Add the missing foreign key between %s and %s with a migration (for example `%s.%s_id references %s(id)`), or change the query to fetch the two tables separately.", orWord(a, "the two tables"), orWord(b, "the table it embeds"), map[bool]string{true: a, false: "child"}[a != ""], map[bool]string{true: b, false: "parent"}[b != ""], map[bool]string{true: b, false: "parent"}[b != ""])
			if ambiguous {
				why = "There is more than one foreign key between the two tables, so Supabase doesn't know which one the query means."
				fix = "Name the relationship in the select, using the foreign key's name or column: `select('*, author:profiles!posts_author_id_fkey(*)')`. Use the hint in the error message for the exact name."
			}
			return Suggestion{
				Severity: c.severity("high"), Confidence: 0.8,
				Title:      fmt.Sprintf("A query joining %s and %s fails", orWord(a, "two tables"), orWord(b, "another table")),
				WhatBroke:  fmt.Sprintf("On %s, a query that loads related data fails, so the page shows nothing.", c.page()),
				Why:        why,
				FixPrompt:  opening(c, n.Message) + "\n\n" + fix + " Don't change unrelated queries.",
				HowToCheck: fmt.Sprintf("Open %s again. The related data should load.", c.page()),
			}
		},
	},
	mk("pg.jwt_expired", reJWTExpired, func(c *Context) bool { n := c.net(); return n != nil && (n.Status == 401 || n.Backend == "postgrest") }, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.75,
			title: "Requests fail for people whose login session expired",
			what:  fmt.Sprintf("On %s, people who kept the app open for a while get an error, because their login token expired and wasn't refreshed.", c.page()),
			why:   "The Supabase client refreshes tokens automatically, unless the app stores the token itself, creates several clients, or sends an old token by hand.",
			fix:   "Use a single Supabase client for the whole app (created once in one file), with the default `autoRefreshToken: true` and `persistSession: true`. Don't copy the access token into state or headers yourself: let the client attach it. Listen to `supabase.auth.onAuthStateChange` so the app reacts when the session ends, and send the person to the login page if refreshing fails.",
			check: "Sign in, leave the app open for more than an hour (or until the token expires), then use it. Requests should still work."}
	}),
	{
		key:   "pg.check_violation",
		match: func(c *Context) bool { return pgCode(c, "23514") },
		build: func(c *Context) Suggestion {
			n := c.net()
			name := ""
			if m := reCheckName.FindStringSubmatch(n.Message); m != nil {
				name = m[1]
			}
			table := tableOr(c, "the table")
			return Suggestion{
				Severity: c.severity("medium"), Confidence: 0.8,
				Title:      fmt.Sprintf("Saving to %s fails a database rule", table),
				WhatBroke:  fmt.Sprintf("On %s, saving is refused because a value breaks a rule on `%s`%s (for example a status that isn't allowed, or a negative amount).", c.page(), table, map[bool]string{true: " (" + quote(name) + ")", false: ""}[name != ""]),
				Why:        "The database only accepts certain values in a column, and the form sends one it doesn't allow.",
				FixPrompt:  opening(c, n.Message) + fmt.Sprintf("\n\nFind the check constraint %s in the migrations for `%s` and see which values it allows. Make the form only offer and send allowed values (and validate them before saving). Only change the constraint if the new value is genuinely valid for the app.", orWord(name, "named in the error"), table),
				HowToCheck: fmt.Sprintf("On %s, save with each option the form offers. All should save.", c.page()),
			}
		},
	},
	{
		key:   "pg.timeout",
		match: func(c *Context) bool { return pgCode(c, "57014") },
		build: func(c *Context) Suggestion {
			table := tableOr(c, "a table")
			return Suggestion{
				Severity: c.severity("high"), Confidence: 0.7,
				Title:      fmt.Sprintf("A database query on %s is too slow and gets cancelled", table),
				WhatBroke:  fmt.Sprintf("On %s, loading data takes so long that the database stops it, so the page shows an error.", c.page()),
				Why:        "The query reads too many rows: no index on the column it filters or sorts by, no limit, or a slow row-level security policy.",
				FixPrompt:  opening(c, c.net().Message) + fmt.Sprintf("\n\nFind this query on `%s`. Add `.limit()` and pagination if it loads everything, select only the columns the page uses, and add an index (in a migration) on the columns it filters and sorts by. If the table has RLS policies that call functions or subqueries, make sure the columns they use are indexed too.", table),
				HowToCheck: fmt.Sprintf("Open %s again. It should load in under a second or two.", c.page()),
			}
		},
	},
	{
		key:   "pg.range",
		match: func(c *Context) bool { return pgCode(c, "PGRST103") || (c.net() != nil && c.net().Status == 416) },
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity: c.severity("low"), Confidence: 0.8,
				Title:      "Asking for a page of results past the end",
				WhatBroke:  fmt.Sprintf("On %s, the app asks for results beyond the last one (for example page 5 of 3), and gets an error instead of an empty list.", c.page()),
				Why:        "The pagination offset is larger than the number of rows.",
				FixPrompt:  opening(c, netLine(c.net())) + "\n\nIn the paginated query (`.range(from, to)`), stop requesting more pages once a page comes back shorter than the page size, and reset to the first page when filters change. Treat this error as \"no more results\".",
				HowToCheck: fmt.Sprintf("On %s, scroll or page to the end of a list, then change a filter. No error should appear.", c.page()),
			}
		},
	},

	// Supabase Storage.
	mk("sb.storage_bucket", regexp.MustCompile(`(?i)Bucket not found`), nil, func(c *Context, line string) spec {
		bucket := ""
		if n := c.net(); n != nil {
			bucket = segment(n.Path, 3)
		}
		return spec{sev: "high", conf: 0.85,
			title: fmt.Sprintf("File uploads fail: the storage bucket %s doesn't exist", orWord(bucket, "used by the app")),
			what:  fmt.Sprintf("On %s, uploading or showing files fails because the storage bucket %s isn't in this Supabase project.", c.page(), orWord(bucket, "named in the request")),
			why:   "The bucket was created in another project or environment, never created, or the name in the code differs.",
			fix:   fmt.Sprintf("Create the bucket %s in the production Supabase project with a migration (`insert into storage.buckets (id, name, public) values ('%s', '%s', false)`) or in the dashboard, and check the name in the code matches exactly (case-sensitive). Add the storage policies the app needs for it.", orWord(bucket, "the code uses"), map[bool]string{true: bucket, false: "<bucket>"}[bucket != ""], map[bool]string{true: bucket, false: "<bucket>"}[bucket != ""]),
			check: fmt.Sprintf("Upload a file on %s. It should succeed and show.", c.page())}
	}),
	mk("sb.storage_rls", regexp.MustCompile(`(?i)row-level security|Unauthorized|not authorized|Access denied`), netWhere("/storage/v1/", 400, 401, 403), func(c *Context, line string) spec {
		bucket := segment(c.net().Path, 3)
		return spec{sev: "high", conf: 0.8,
			title: fmt.Sprintf("Uploading files is blocked by storage permissions%s", map[bool]string{true: " on " + bucket, false: ""}[bucket != ""]),
			what:  fmt.Sprintf("On %s, uploading or reading a file is refused by Supabase Storage's security rules.", c.page()),
			why:   "Supabase Storage keeps files in `storage.objects`, protected by row-level security. Without a policy allowing this action for this user, it's refused.",
			fix:   fmt.Sprintf("Add a storage policy with a migration on `storage.objects` for the bucket %s that allows exactly this action (insert for uploads, select for reads, update when upserting) for signed-in users, ideally only in their own folder: `bucket_id = '%s' and (storage.foldername(name))[1] = auth.uid()::text`, and make the app upload to `<user id>/<file name>`. Don't make the bucket public just to fix this.", orWord(bucket, "used"), map[bool]string{true: bucket, false: "<bucket>"}[bucket != ""]),
			check: fmt.Sprintf("Sign in as a normal user and upload a file on %s. It should succeed.", c.page())}
	}),
	mk("sb.storage_exists", regexp.MustCompile(`(?i)The resource already exists|Duplicate|already exists`), netWhere("/storage/v1/", 400, 409), func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.85,
			title: "Uploading a file fails when one with the same name exists",
			what:  fmt.Sprintf("On %s, uploading fails because a file with the same path is already stored.", c.page()),
			why:   "Uploads don't overwrite existing files unless asked to, and the app uses the same file name every time (for example \"avatar.png\").",
			fix:   "In the upload call, either pass `{ upsert: true }` when replacing the file is intended (and allow `update` in the storage policy), or make each path unique (for example `${userId}/${Date.now()}-${file.name}`). Pick the one that matches what the feature should do.",
			check: fmt.Sprintf("On %s, upload a file twice with the same name. Both should succeed.", c.page())}
	}),
	mk("sb.storage_too_large", regexp.MustCompile(`(?i)Payload too large|exceeded the maximum allowed size|object exceeded`), netWhere("/storage/v1/", 400, 413), func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.85,
			title: "Uploads fail for files that are too big",
			what:  fmt.Sprintf("On %s, uploading a large file fails because it's over the storage size limit.", c.page()),
			why:   "Supabase limits upload size (per project and per bucket), and the app doesn't check the size first.",
			fix:   "Check the file size in the browser before uploading and show a clear message with the maximum (or compress images first). If larger files are really needed, raise the bucket's file size limit in Supabase Storage settings, and use resumable uploads for files over 6 MB.",
			check: fmt.Sprintf("On %s, choose a very large file. You should see a clear size message instead of an error.", c.page())}
	}),

	// Supabase Auth.
	mk("auth.link_expired", reOTPExpired, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.75,
			title: "Email sign-in or confirmation links don't work",
			what:  fmt.Sprintf("On %s, people open the link from their email and get \"%s\", so they can't sign in or confirm their account.", c.page(), line),
			why:   "The link was already used (some email scanners open links automatically), it expired, it was opened in a different browser than the one that requested it (PKCE flow), or the redirect URL isn't allowed.",
			fix:   "Check Supabase Authentication > URL Configuration: the Site URL is the production domain, and the redirect URLs include the page the link returns to. In the code, handle the error returned in the link's URL by showing \"This link expired\" with a button to send a new one. If people often open the link on another device, use a one-time code (OTP) instead of a magic link.",
			check: "Request a new link and open it in the same browser. It should sign you in. Opening it again should show a friendly \"expired\" message."}
	}),
	{
		key: "auth.config",
		match: func(c *Context) bool {
			return authCode(c, "signup_disabled", "email_provider_disabled", "provider_disabled", "phone_provider_disabled", "anonymous_provider_disabled")
		},
		build: func(c *Context) Suggestion {
			n := c.net()
			return Suggestion{
				Severity: c.severity("high"), Confidence: 0.85,
				Title:      "Sign-up or a sign-in method is turned off in Supabase",
				WhatBroke:  fmt.Sprintf("On %s, people can't sign up or sign in, because Supabase answers \"%s\" (%s).", c.page(), n.Message, n.Code),
				Why:        "The sign-up or the sign-in method the app uses is disabled in the Supabase project's settings.",
				FixPrompt:  opening(c, n.Message) + fmt.Sprintf("\n\nThis is a setting, not code: in the Supabase dashboard, Authentication > Providers (or Sign In / Up), enable the method the app uses (error code `%s`). If sign-ups are meant to be closed, change the app to hide the sign-up form and show a clear message instead.", n.Code),
				HowToCheck: fmt.Sprintf("Sign up with a new account on %s. It should work, or show the intended message.", c.page()),
			}
		},
	},
	{
		key:   "auth.invalid_email",
		match: func(c *Context) bool { return authCode(c, "email_address_invalid", "email_address_not_authorized") },
		build: func(c *Context) Suggestion {
			n := c.net()
			notAuth := n.Code == "email_address_not_authorized"
			why := "The email address is rejected as invalid; often a typo, spaces around it, or a domain Supabase considers undeliverable."
			fix := "Trim spaces and validate the email format in the form before calling Supabase, and show the error message next to the email field instead of a generic error."
			if notAuth {
				why = "The project uses Supabase's built-in email service, which only sends to the team's own addresses. Emails to anyone else are refused."
				fix = "This is a setting: configure a custom SMTP provider in Supabase (Authentication > Emails > SMTP Settings), for example Resend, SendGrid or Postmark, so emails reach real users."
			}
			return Suggestion{
				Severity: c.severity(map[bool]string{true: "high", false: "low"}[notAuth]), Confidence: 0.8,
				Title:      "Sign-up emails are refused",
				WhatBroke:  fmt.Sprintf("On %s, Supabase refuses the email address (\"%s\").", c.page(), n.Message),
				Why:        why,
				FixPrompt:  opening(c, n.Message) + "\n\n" + fix,
				HowToCheck: fmt.Sprintf("Sign up on %s with a normal address (and one with spaces around it). Both should work.", c.page()),
			}
		},
	},
	{
		key:   "auth.same_password",
		match: func(c *Context) bool { return authCode(c, "same_password") },
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity: "low", Confidence: 0.9,
				Title:      "Changing a password to the same one shows an error",
				WhatBroke:  fmt.Sprintf("On %s, someone tried to set the password they already have, and saw a raw error.", c.page()),
				Why:        "Supabase refuses a new password that equals the current one; the app doesn't explain it.",
				FixPrompt:  opening(c, c.net().Message) + "\n\nWhere the password is updated (`supabase.auth.updateUser({ password })`), handle the `same_password` error by showing \"Your new password must be different from the current one\" next to the field.",
				HowToCheck: fmt.Sprintf("On %s, set the same password again. A clear message should appear.", c.page()),
			}
		},
	},
	{
		key:   "auth.rate_limit",
		match: func(c *Context) bool { return authCode(c, "over_request_rate_limit", "over_sms_send_rate_limit") },
		build: func(c *Context) Suggestion {
			return Suggestion{
				Severity: c.severity("medium"), Confidence: 0.8,
				Title:      "Sign-in requests are being rate limited",
				WhatBroke:  fmt.Sprintf("On %s, Supabase refuses sign-in requests because too many came from the same place in a short time.", c.page()),
				Why:        "The app sends auth requests repeatedly (a loop, a retry without delay, or a component that re-runs), or many people share one IP.",
				FixPrompt:  opening(c, c.net().Message) + "\n\nFind where this auth call is made and make sure it runs only on a user action, once: disable the button while it's pending, and don't call it from an effect that re-runs. Show \"Too many attempts, please wait a minute\" when this error comes back. If the limits are too low for real traffic, raise them in Supabase Authentication > Rate Limits.",
				HowToCheck: fmt.Sprintf("Sign in on %s and click the button several times quickly. Only one request should be sent.", c.page()),
			}
		},
	},

	// Firebase.
	mk("fb.permission_denied", reFBPermission, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.8,
			title: "Firebase security rules block the app from reading or saving data",
			what:  fmt.Sprintf("On %s, Firestore or Storage refuses the request (\"%s\"), so data doesn't load or save.", c.page(), line),
			why:   "The security rules don't allow this read or write for this user, or the user isn't signed in yet when the request runs.",
			fix:   "Find the collection or path this request uses and update `firestore.rules` (or `storage.rules`) to allow exactly this action for the right users, for example `allow read, write: if request.auth != null && request.auth.uid == resource.data.ownerId;` (use `request.resource.data` for creates). In the code, wait for `onAuthStateChanged` to report the user before querying. Don't open the rules to everyone.",
			check: fmt.Sprintf("Sign in as a normal user and use %s. Data should load and save.", c.page())}
	}),
	mk("fb.index_required", reFBIndex, nil, func(c *Context, line string) spec {
		link := ""
		for _, t := range c.texts() {
			if l := reFBIndexLink.FindString(t); l != "" {
				link = strings.TrimRight(l, ".,)\"'")
				break
			}
		}
		create := "Create the index the error describes in the Firebase console (Firestore > Indexes), or add it to `firestore.indexes.json` and deploy it with `firebase deploy --only firestore:indexes`."
		if link != "" {
			create = "Open this link while signed in to the Firebase project; it creates the exact index the query needs: " + link + ". Also add it to `firestore.indexes.json` so it's kept in the code."
		}
		return spec{sev: "high", conf: 0.9,
			title: "A Firestore query needs an index that doesn't exist yet",
			what:  fmt.Sprintf("On %s, a query that filters and sorts data fails until Firestore has an index for it.", c.page()),
			why:   "Firestore requires a composite index for queries that combine several filters, or a filter with a sort on another field.",
			fix:   create + " Index building takes a few minutes. No code change is needed.",
			check: fmt.Sprintf("Once the index shows as enabled, open %s again. The data should load.", c.page())}
	}),
	mk("fb.auth_credentials", reFBCredential, nil, func(c *Context, line string) spec {
		return spec{sev: "low", conf: 0.85,
			title: "Wrong email or password shows a raw Firebase error",
			what:  fmt.Sprintf("On %s, someone entered a wrong email or password and saw \"%s\" instead of a clear message.", c.page(), line),
			why:   "Wrong credentials are normal; the app doesn't turn Firebase's error code into a friendly message.",
			fix:   "In the sign-in handler, catch Firebase auth errors and map `auth/invalid-credential`, `auth/wrong-password` and `auth/user-not-found` to \"Email or password is incorrect\" (don't say which one), shown next to the form. Keep the button usable for another try and offer a password reset link.",
			check: fmt.Sprintf("Sign in on %s with a wrong password. A clear message should appear.", c.page())}
	}),
	mk("fb.auth_email_in_use", reFBEmailInUse, nil, func(c *Context, line string) spec {
		return spec{sev: "low", conf: 0.85,
			title: "Signing up with an existing email shows a raw error",
			what:  fmt.Sprintf("On %s, someone tried to sign up with an email that already has an account and saw \"%s\".", c.page(), line),
			why:   "The account exists; the app doesn't explain it or offer to sign in.",
			fix:   "Catch `auth/email-already-in-use` in the sign-up handler and show \"This email already has an account\" with links to sign in and reset the password.",
			check: fmt.Sprintf("Sign up on %s with an existing email. A clear message with a sign-in link should appear.", c.page())}
	}),
	mk("fb.auth_weak_password", reFBWeak, nil, func(c *Context, line string) spec {
		return spec{sev: "low", conf: 0.85,
			title: "Sign-up fails with a too-short password and a raw error",
			what:  fmt.Sprintf("On %s, sign-up fails because the password is too weak, shown as \"%s\".", c.page(), line),
			why:   "Firebase requires at least 6 characters (or the project's password policy); the form doesn't check it first.",
			fix:   "Validate the password in the form before calling Firebase (at least the required length, shown as a hint under the field), and map `auth/weak-password` to a clear message.",
			check: fmt.Sprintf("Sign up on %s with a 3-character password. The form should explain the rule.", c.page())}
	}),
	mk("fb.unauthorized_domain", reFBDomain, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.9,
			title: "Sign-in doesn't work on the published domain",
			what:  fmt.Sprintf("On %s, Google (or another provider) sign-in fails because the site's domain isn't allowed in Firebase.", c.page()),
			why:   "Firebase only allows sign-in popups and redirects from the domains listed in its settings, and the production domain isn't there.",
			fix:   "This is a setting: in the Firebase console, Authentication > Settings > Authorized domains, add the production domain (and any custom or preview domains). No code change is needed.",
			check: "On the published site, sign in with the provider. It should work."}
	}),
	mk("fb.popup", reFBPopup, nil, func(c *Context, line string) spec {
		return spec{sev: "low", conf: 0.8,
			title: "Closing the sign-in popup is reported as an error",
			what:  fmt.Sprintf("On %s, someone closed the sign-in popup (or the browser blocked it), and the app treated it as a failure.", c.page()),
			why:   "Closing or blocking the popup throws an error; the app doesn't expect it.",
			fix:   "In the popup sign-in handler, ignore `auth/popup-closed-by-user` and `auth/cancelled-popup-request` (just re-enable the button). For `auth/popup-blocked`, fall back to `signInWithRedirect`. Only call the popup directly from a click.",
			check: fmt.Sprintf("On %s, open the sign-in popup and close it. No error should appear and the button should work again.", c.page())}
	}),

	// React and the browser.
	mk("js.hydration", reHydration, nil, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.7,
			title: fmt.Sprintf("%s renders differently on the server and in the browser", capFirst(c.page())),
			what:  fmt.Sprintf("On %s, the HTML from the server doesn't match what React renders in the browser, so React throws it away and re-renders (\"%s\").", c.page(), line),
			why:   "Something renders differently on each side: dates, times or random values, `window`/`localStorage` checks during render, or invalid HTML nesting such as a `<div>` inside a `<p>`.",
			fix:   "Find what renders differently: values from `Date.now()`, `new Date().toLocaleString()`, `Math.random()`, `window`, `localStorage` or the user's locale used directly in render. Move them into a `useEffect` (render a placeholder first), or mark the component client-only. Also fix invalid nesting (no block elements inside `<p>`, no `<a>` inside `<a>`). Change only the component that differs.",
			check: fmt.Sprintf("Open %s in a fresh tab with the console open. There should be no hydration warning.", c.page())}
	}),
	mk("js.render_loop", reRenderLoop, nil, func(c *Context, line string) spec {
		near := ""
		if j := c.js(); j != nil && j.Function != "" {
			near = fmt.Sprintf(" The crash is near `%s`.", j.Function)
		}
		return spec{sev: "high", conf: 0.75,
			title: fmt.Sprintf("%s gets stuck re-rendering and crashes", capFirst(c.page())),
			what:  fmt.Sprintf("On %s, a component updates its state on every render, so React stops it (\"%s\").", c.page(), line),
			why:   "State is set during render, or a `useEffect` sets state that is also in its own dependency list (or depends on an object or array that is new on every render).",
			fix:   "Find the component that loops." + near + " Look for `setState` called directly in the component body or in `onClick={handler()}` (should be `onClick={() => handler()}`), and for effects whose dependencies change because they set them, or because they are objects or functions created during render (wrap those in `useMemo`/`useCallback`, or depend on a primitive like an id). Fix only that component.",
			check: fmt.Sprintf("Open %s with the console open and use it. It shouldn't freeze or crash.", c.page())}
	}),
	mk("js.hooks_order", reHooks, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.75,
			title: fmt.Sprintf("%s crashes because React hooks run in a different order", capFirst(c.page())),
			what:  fmt.Sprintf("On %s, a component crashes with \"%s\".", c.page(), line),
			why:   "A hook (`useState`, `useEffect`, ...) is called conditionally, after an early `return`, inside a loop, or outside a component; or two copies of React are bundled.",
			fix:   "Find the component in this error and move every hook call to the top of the component, before any `if`/early `return`, and never inside loops or callbacks. If the error is \"Invalid hook call\", also check that only one version of `react` and `react-dom` is installed (`npm ls react`). Keep the component's behaviour the same.",
			check: fmt.Sprintf("Open %s and go through its loading, empty and loaded states. It shouldn't crash.", c.page())}
	}),
	mk("js.no_provider", reNoProvider, nil, func(c *Context, line string) spec {
		hook, provider := "", ""
		if m := reNoProvider.FindStringSubmatch(line); m != nil {
			hook, provider = m[1]+m[3], m[2]+m[4]
		}
		return spec{sev: "high", conf: 0.8,
			title: fmt.Sprintf("%s crashes: a component is used outside its provider", capFirst(c.page())),
			what:  fmt.Sprintf("On %s, a component uses %s, but it isn't inside the %s it needs (\"%s\").", c.page(), orWord(hook, "a context hook"), orWord(provider, "provider"), line),
			why:   "The page or component is rendered outside the provider (router, query client, auth or theme context) that the hook reads, often after moving a route or rendering a component in a portal or a new root.",
			fix:   fmt.Sprintf("Find where %s is used on this page and make sure that component is rendered inside %s. Usually the fix is to move the provider up so it wraps the whole app in the root file (for example `main.tsx` or `App.tsx`), or to move the route inside the router. Don't remove the hook.", orWord(hook, "the hook"), orWord(provider, "its provider")),
			check: fmt.Sprintf("Open %s directly (paste its URL in a new tab). It should load.", c.page())}
	}),
	mk("js.object_child", reObjectChild, nil, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.8,
			title: fmt.Sprintf("%s crashes trying to show an object as text", capFirst(c.page())),
			what:  fmt.Sprintf("On %s, the page tries to display a whole object (or a Date, or an error) where text should go, and React crashes.", c.page()),
			why:   "A value that is an object is rendered directly, for example `{user}` instead of `{user.name}`, a Date instead of `date.toLocaleDateString()`, or an API error object instead of its message.",
			fix:   "Find the JSX on this page that renders a value which can be an object (the error message lists its keys). Render the right field (`.name`, `.message`, `.toLocaleDateString()`), or `JSON.stringify` it if it's only for debugging. Keep the change to that spot.",
			check: fmt.Sprintf("Open %s, including when the data has errors. It shouldn't crash.", c.page())}
	}),
	mk("js.invalid_date", reInvalidDate, isBrowser, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.7,
			title: fmt.Sprintf("%s crashes formatting a missing or invalid date", capFirst(c.page())),
			what:  fmt.Sprintf("On %s, formatting a date fails with \"%s\".", c.page(), line),
			why:   "The date is empty, null, or in a format JavaScript can't read (for example \"25/11/2026\"), and the code formats it without checking (`format()` from date-fns or `toISOString()` throw on invalid dates).",
			fix:   "Find where dates are formatted on this page. Before formatting, check the value exists and is valid (`date && !isNaN(new Date(date).getTime())`, or date-fns `isValid`), show \"-\" otherwise. Parse dates from the database as ISO strings. Keep the change to the formatting code.",
			check: fmt.Sprintf("Open %s with records that have no date. It should show a placeholder instead of crashing.", c.page())}
	}),
	mk("js.stack_overflow", reStackOverflow, nil, func(c *Context, line string) spec {
		near := ""
		if j := c.js(); j != nil && j.Function != "" {
			near = fmt.Sprintf(" (near `%s`)", j.Function)
		} else if s := c.srv(); s != nil && s.Function != "" {
			near = fmt.Sprintf(" (in `%s`)", s.Function)
		}
		return spec{sev: "high", conf: 0.65,
			title: "A function calls itself forever until it crashes",
			what:  fmt.Sprintf("On %s, the %s crashes with \"%s\".", c.page(), whereNamed(c), line),
			why:   "Infinite recursion: a function calls itself (directly, or through a getter, a setter, a `toJSON`, or two functions calling each other) without a stopping condition, or circular data is walked or serialized.",
			fix:   "Find the function that repeats in the stack trace" + near + ". Add the missing stopping condition, fix the property that calls itself (a setter assigning to its own property instead of a backing field), or break the cycle when walking or serializing circular data. Keep the change to that function.",
			check: fmt.Sprintf("Repeat what failed on %s. It should complete.", c.page())}
	}),
	mk("js.storage_quota", reQuota, isBrowser, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.8,
			title: "Saving to the browser's storage fails because it's full",
			what:  fmt.Sprintf("On %s, writing to localStorage fails (\"%s\"), so settings or cached data aren't saved.", c.page(), line),
			why:   "localStorage holds about 5 MB per site. The app stores large data there (images, whole API responses, growing logs) or never cleans up.",
			fix:   "Find what the app writes to `localStorage`. Don't store large data there: keep only small settings and IDs, move big data to IndexedDB or the server. Wrap writes in try/catch so a full storage never breaks the page, and remove old keys the app no longer uses.",
			check: fmt.Sprintf("Use %s for a while. Nothing should fail, and stored data should stay small.", c.page())}
	}),
}

var (
	reEmail       = regexp.MustCompile(`(?i)SmtpException|Failure sending mail|SMTPAuthenticationError|SMTPServerDisconnected|SMTPConnectError|SMTPRecipientsRefused|smtplib\.|nodemailer|Invalid login: 535|535[ -]5\.7\.\d|Greeting never received|Connection could not be established with host|Expected response code "?250|Swift_TransportException|TransportException.*smtp|MailKit|Mail(?:er)?Exception|EAUTH|(?:ECONNREFUSED|ETIMEDOUT|ECONNRESET)[^\n]*:(?:25|465|587|2525)\b|domain is not verified|not a verified Sender Identity|from address does not match`)
	reDBAuth      = regexp.MustCompile(`(?i)password authentication failed for user|Login failed for user|Access denied for user|28P01|no pg_hba\.conf entry|SASL authentication failed|Authentication failed\.?.*(?:mongo|database)`)
	reTLS         = regexp.MustCompile(`(?i)certificate has expired|unable to verify the first certificate|self[- ]signed certificate|CERTIFICATE_VERIFY_FAILED|The remote certificate is invalid|PKIX path building failed|x509: certificate|SSL routines|UNABLE_TO_GET_ISSUER_CERT|ERR_TLS_CERT_ALTNAME_INVALID|Hostname/IP does not match certificate`)
	reMissingConf = regexp.MustCompile(`(?i)Missing required environment variable|environment variable \S+ (?:is not set|is required|is missing|not found)|ConnectionString property has not been initialized|secretOrPrivateKey must have a value|The ['"]?\w+['"]? environment variable is missing|Configuration value .* (?:is missing|not found)|Neither apiKey nor config\.authenticator provided|ImproperlyConfigured`)
	reEnvKeyError = regexp.MustCompile(`^KeyError: '[A-Z][A-Z0-9_]{2,}'$`)
	reJWTServer   = regexp.MustCompile(`(?i)TokenExpiredError|jwt expired|JsonWebTokenError|invalid signature|SecurityTokenExpiredException|SecurityTokenInvalidSignatureException|SecurityTokenSignatureKeyNotFoundException|ExpiredSignatureError|InvalidSignatureError|DecodeError: .*token|ExpiredJwtException|SignatureException`)
	reDeadlock    = regexp.MustCompile(`(?i)deadlock detected|Deadlock found when trying to get lock|was deadlocked on lock resources|40P01|could not serialize access due to|Lock wait timeout exceeded`)
	reNotFound    = regexp.MustCompile(`(?i)Sequence contains no elements|Sequence contains more than one (?:matching )?element|matching query does not exist|DoesNotExist|NoResultFound|No row was found|EntityNotFoundException|ModelNotFoundException|RecordNotFound|EmptyResultDataAccessException|NoSuchElementException|IndexError: list index out of range`)
	reFileMissing = regexp.MustCompile(`(?i)ENOENT|FileNotFoundException|FileNotFoundError|No such file or directory|DirectoryNotFoundException|Could not find (?:a part of )?the (?:file|path)`)
	reReadOnlyFS  = regexp.MustCompile(`(?i)EROFS|read-only file system|EACCES: permission denied|Access to the path .* is denied|PermissionError: \[Errno 13\]`)
	reOOM         = regexp.MustCompile(`(?i)OutOfMemoryError|JavaScript heap out of memory|MemoryError|OutOfMemoryException|Allocation failed|Allowed memory size of \d+ bytes exhausted|fatal error: runtime: out of memory`)
	reModule      = regexp.MustCompile(`(?i)Cannot find module ['"]([^'"]+)['"]|ModuleNotFoundError: No module named ['"]([^'"]+)['"]|No module named ['"]?([\w.]+)|Could not load file or assembly ['"]?([\w.]+)|ClassNotFoundException: ([\w.$]+)|NoClassDefFoundError: ([\w/$]+)|Class ["']([\w\\]+)["'] not found|ERR_MODULE_NOT_FOUND`)
	reJSONBody    = regexp.MustCompile(`(?i)Unexpected token .* in JSON|is not valid JSON|Unexpected end of JSON input|JSONDecodeError|JsonException|JsonReaderException|JsonSerializationException|JsonParseException|HttpMessageNotReadableException|invalid character .* looking for beginning of value`)
	reBodyTooBig  = regexp.MustCompile(`(?i)request entity too large|PayloadTooLargeError|Request body too large|Maximum request length exceeded|RequestDataTooBig|POST Content-Length of \d+ bytes exceeds|MaxUploadSizeExceededException|http: request body too large`)
	reCSRF        = regexp.MustCompile(`(?i)CSRF token mismatch|TokenMismatchException|antiforgery token|AntiforgeryValidationException|CSRF verification failed|invalid csrf token|CSRF token missing`)
	reEFTracking  = regexp.MustCompile(`cannot be tracked because another instance with the (?:same )?key value`)
	reConcurrency = regexp.MustCompile(`(?i)DbUpdateConcurrencyException|optimistic concurrency|StaleObjectStateException|OptimisticLockException|StaleDataError|ObjectOptimisticLockingFailureException`)
	reDiskFull    = regexp.MustCompile(`(?i)No space left on device|ENOSPC|could not extend file|disk full|There is not enough space on the disk`)
	rePortInUse   = regexp.MustCompile(`(?i)EADDRINUSE|address already in use|Only one usage of each socket address|Failed to bind to address`)
	reSqlNull     = regexp.MustCompile(`(?i)SqlNullValueException|Data is Null\. This method or property cannot be called on Null values|Nullable object must have a value`)
	reTimeout     = regexp.MustCompile(`(?i)ETIMEDOUT|ESOCKETTIMEDOUT|socket hang up|ECONNRESET|ReadTimeout|ConnectTimeout|ConnectTimeoutError|HttpClient\.Timeout|TaskCanceledException.*(?:timeout|canceled)|context deadline exceeded|i/o timeout|timed out|SocketTimeoutException|Gateway Time-?out`)
)

var serverFirst = []entry{
	mk("srv.email_send", reEmail, nil, func(c *Context, line string) spec {
		hint := ""
		if s := c.srv(); s != nil && s.Platform == "csharp" {
			hint = " .NET's built-in `SmtpClient` doesn't support implicit TLS on port 465: use port 587 with `EnableSsl = true`, or switch to MailKit."
		}
		return spec{sev: "high", conf: 0.75,
			title: "Sending email fails",
			what:  fmt.Sprintf("On %s, the %s can't send an email (\"%s\"), so confirmations, resets or notifications don't arrive and the request may fail.", c.page(), whereNamed(c), line),
			why:   "The mail settings don't match the mail provider: wrong port or encryption (465 = SSL from the start, 587 = STARTTLS), wrong username or password (many providers need an app password or API key), the hosting blocks outgoing SMTP, or the sender domain isn't verified.",
			fix:   "Find where email is sent and the settings it reads. Check host, port and encryption against the provider's documentation, the username and password (use an app password or the provider's SMTP API key), and that the From address is on a verified domain." + hint + " If the hosting blocks SMTP ports, use the provider's HTTP API instead (Resend, SendGrid, Postmark, Mailgun). Also make the request succeed (or show a clear message) when the email can't be sent, instead of an error page, and log the failure.",
			check: "Trigger the email again (sign up or reset a password). It should arrive, and no error should be reported."}
	}),
	mk("srv.db_auth", reDBAuth, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.85,
			title: "The server can't log in to its database",
			what:  fmt.Sprintf("On %s, the server is refused by the database (\"%s\"), so everything that needs data fails.", c.page(), line),
			why:   "The database username or password in production is wrong or was changed, the user lacks access to this database, or the connection string points at the wrong server.",
			fix:   "Check the production connection string or database URL (environment variable or secret): host, port, database name, username and password. Special characters in the password must be URL-encoded in a URL. If the password was rotated, update the secret and restart the app. Don't put the password in the code.",
			check: fmt.Sprintf("After redeploying, open %s. It should load data.", c.page())}
	}),
	mk("srv.tls", reTLS, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.75,
			title: "A secure connection fails because of a certificate problem",
			what:  fmt.Sprintf("On %s, the %s can't connect to a service over HTTPS/TLS (\"%s\").", c.page(), whereNamed(c), line),
			why:   "The other side's certificate expired or doesn't match the host name, or it's self-signed or from an internal authority the server doesn't trust, or the server's own CA certificates are outdated.",
			fix:   "Find which host the failing connection goes to. If it's your own service, renew or fix its certificate (it must cover the exact host name). If it's a database or internal service with its own CA, give the client that CA certificate (for example `ssl: { ca }` / `sslrootcert`). Update the server's CA certificates package if they're old. Don't disable certificate verification.",
			check: fmt.Sprintf("Repeat what failed on %s. The connection should succeed.", c.page())}
	}),
	{
		key: "srv.missing_config",
		match: func(c *Context) bool {
			if c.found(reMissingConf) != "" {
				return true
			}
			for _, t := range c.texts() {
				if reEnvKeyError.MatchString(strings.TrimSpace(t)) {
					return true
				}
			}
			return false
		},
		build: func(c *Context) Suggestion {
			line := c.found(reMissingConf)
			if line == "" {
				for _, t := range c.texts() {
					if reEnvKeyError.MatchString(strings.TrimSpace(t)) {
						line = strings.TrimSpace(t)
					}
				}
			}
			return Suggestion{
				Severity: c.severity("high"), Confidence: 0.8,
				Title:      "A setting the server needs is missing in production",
				WhatBroke:  fmt.Sprintf("On %s, the server stops because a required setting (secret, key or connection string) isn't set: \"%s\".", c.page(), line),
				Why:        "The value exists on the developer's machine (a .env file or local settings) but wasn't added to the production environment.",
				FixPrompt:  opening(c, line) + "\n\nFind the exact name of the setting the code reads at this point. Add it to the production environment (hosting dashboard, secrets manager or app settings) with the right value, and redeploy or restart. Also make the app check required settings at startup and fail with a clear message listing what's missing. Don't hard-code the value.",
				HowToCheck: fmt.Sprintf("After redeploying, repeat what failed on %s. It should work.", c.page()),
			}
		},
	},
	mk("srv.jwt", reJWTServer, isServer, func(c *Context, line string) spec {
		expired := regexp.MustCompile(`(?i)expired`).MatchString(line)
		why := "The token's signature doesn't match the key the server checks it with: a different secret in production, a token from another environment, or a rotated key."
		fix := "Make sure the server verifies tokens with the same secret or public key that issued them, read from configuration (for Supabase tokens: the project's JWT secret, or its JWKS). Return 401 for invalid tokens instead of an error page."
		if expired {
			why = "The login token expired, and the server treats that as a crash instead of asking the client to refresh or sign in again."
			fix = "Catch expired-token errors where tokens are verified and return 401 (not 500), so the client can refresh the token or show the sign-in page. Make sure the client refreshes tokens before they expire."
		}
		return spec{sev: "medium", conf: 0.75,
			title: "Requests fail when a login token is expired or invalid",
			what:  fmt.Sprintf("On %s, checking the login token fails (\"%s\") and the request errors.", c.page(), line),
			why:   why, fix: fix,
			check: fmt.Sprintf("Call %s with an expired token. It should answer 401 and the app should ask to sign in.", c.page())}
	}),
	mk("srv.deadlock", reDeadlock, nil, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.7,
			title: "Two requests block each other in the database",
			what:  fmt.Sprintf("On %s, a database transaction is cancelled because it was waiting on another one (\"%s\").", c.page(), line),
			why:   "Two transactions lock the same rows in a different order, or hold locks too long (slow work inside the transaction).",
			fix:   "In the code that runs this transaction: lock or update rows in a consistent order everywhere (for example by id), keep transactions short (no HTTP calls or slow work inside), and retry the whole transaction once or twice when this specific error happens.",
			check: fmt.Sprintf("Repeat the action on %s from two tabs at the same time. Both should succeed.", c.page())}
	}),
	mk("srv.not_found", reNotFound, isServer, func(c *Context, line string) spec {
		many := regexp.MustCompile(`(?i)more than one`).MatchString(line)
		why := "The code assumes a record exists (`.First()`, `.get()`, `[0]`, `findOrFail`) and crashes when it doesn't: a wrong or old ID in the URL, a deleted record, or a record of another user."
		fix := "At this spot, use the variant that allows no result (`FirstOrDefault`, `.first()`, `filter().first()`, `find` instead of `findOrFail`, or a length check) and return a 404 \"not found\" page or response when it's missing."
		if many {
			why = "The code expects exactly one matching record (`.Single()`), but the database has several (duplicates)."
			fix = "Decide whether duplicates are valid. If not, clean them up and add a unique constraint with a migration; if yes, use `First` with an explicit order instead of `Single`."
		}
		return spec{sev: "medium", conf: 0.75,
			title: fmt.Sprintf("%s fails when the record it looks for %s", capFirst(c.page()), map[bool]string{true: "exists more than once", false: "doesn't exist"}[many]),
			what:  fmt.Sprintf("On %s, the server crashes looking up a record (\"%s\").", c.page(), line),
			why:   why, fix: fix + " Keep the change to that spot.",
			check: fmt.Sprintf("Open %s with an ID that doesn't exist. It should show \"not found\", not an error page.", c.page())}
	}),
	mk("srv.sql_null", reSqlNull, nil, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.75,
			title: "Reading data fails when a database value is empty",
			what:  fmt.Sprintf("On %s, the server crashes reading a record because a column is NULL (\"%s\").", c.page(), line),
			why:   "A model property is declared as required (non-nullable) but the database column contains NULL for some rows.",
			fix:   "Find which column is NULL for the failing rows (compare the model with the table). Either make that property nullable in the model (`string?`, `int?`, `DateTime?`) and handle the empty case where it's used, or fill the NULLs with a default and make the column NOT NULL in a migration.",
			check: fmt.Sprintf("Open %s for records that have empty fields. They should load.", c.page())}
	}),
	{
		key:   "srv.module_missing",
		match: func(c *Context) bool { return isServer(c) && c.found(reModule) != "" },
		build: func(c *Context) Suggestion {
			line := c.found(reModule)
			name := ""
			if m := reModule.FindStringSubmatch(line); m != nil {
				for _, g := range m[1:] {
					if g != "" {
						name = g
						break
					}
				}
			}
			return Suggestion{
				Severity: c.severity("high"), Confidence: 0.8,
				Title:      fmt.Sprintf("A dependency %s is missing on the server", orWord(name, "the code needs")),
				WhatBroke:  fmt.Sprintf("On %s, the server can't load %s, so this code doesn't run.", c.page(), orWord(name, "a package or module")),
				Why:        "The package is installed locally but not declared in the project's dependency file, was installed as a dev-only dependency, isn't copied into the deployed build, or the import path's capitalization differs (production servers are case-sensitive).",
				FixPrompt:  opening(c, line) + fmt.Sprintf("\n\nAdd %s to the project's dependencies (package.json `dependencies`, requirements.txt or pyproject, the .csproj, pom.xml or build.gradle, composer.json), not as a dev-only dependency. If it's a file of the project, check the import path matches the file name exactly, including capitalization. Then redeploy.", orWord(name, "the missing package")),
				HowToCheck: fmt.Sprintf("After redeploying, repeat what failed on %s. It should work.", c.page()),
			}
		},
	},
	mk("srv.file_missing", reFileMissing, isServer, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.7,
			title: "The server can't find a file it needs",
			what:  fmt.Sprintf("On %s, the server tries to open a file that isn't there (\"%s\").", c.page(), line),
			why:   "The path is relative to a different working directory in production, the file isn't included in the deployed build, or it was written to local disk on another instance.",
			fix:   "Find the file path in the code at this point. Build it from the app's own directory (for example `path.join(__dirname, ...)`, `Path.Combine(AppContext.BaseDirectory, ...)`, `Path(__file__).parent`) instead of the working directory, and make sure the file is included in the deployment. Files created at runtime (uploads, generated reports) belong in object storage, not on the server's disk.",
			check: fmt.Sprintf("After redeploying, repeat what failed on %s. It should work.", c.page())}
	}),
	mk("srv.readonly_fs", reReadOnlyFS, isServer, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.75,
			title: "The server isn't allowed to write files where it tries to",
			what:  fmt.Sprintf("On %s, writing a file fails (\"%s\").", c.page(), line),
			why:   "Serverless and container hosts make the app's folder read-only (only a temporary folder is writable, and it isn't kept), or the app's user lacks permission.",
			fix:   "Don't write to the app's folder in production. Store uploads and generated files in object storage (Supabase Storage, S3, Cloudinary) and keep only a reference in the database; use the system temp folder (`os.tmpdir()`, `Path.GetTempPath()`, `tempfile`) for short-lived files.",
			check: fmt.Sprintf("Repeat the upload or export on %s. It should work.", c.page())}
	}),
	mk("srv.out_of_memory", reOOM, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.65,
			title: "The server runs out of memory",
			what:  fmt.Sprintf("On %s, the %s runs out of memory and crashes (\"%s\").", c.page(), whereNamed(c), line),
			why:   "Something loads too much at once: a whole table, a big file read into memory, an unbounded cache or list, or a memory leak that grows over time.",
			fix:   "Find what this request loads. Use pagination or streaming instead of loading everything (database queries with limits, streaming file uploads and downloads), bound caches, and process big jobs in batches. Only raise the memory limit after that, if the work really needs it.",
			check: fmt.Sprintf("Repeat the action on %s with large data. Memory should stay stable and the request should complete.", c.page())}
	}),
	mk("srv.bad_json", reJSONBody, isServer, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.7,
			title: "The server crashes on a request body that isn't valid JSON",
			what:  fmt.Sprintf("On %s, the server fails to read the request's JSON (\"%s\").", c.page(), line),
			why:   "The client sends an empty body, form data or text where JSON is expected, the Content-Type doesn't match, or the server parses an error page from another service as JSON.",
			fix:   "Find where this body is parsed. Return a 400 response with a clear message for invalid or empty JSON instead of crashing, and make the client send `JSON.stringify(data)` with `Content-Type: application/json`. If the JSON comes from another service, check its status and content type before parsing.",
			check: fmt.Sprintf("Send an empty and an invalid body to %s. It should answer 400, not 500.", c.page())}
	}),
	mk("srv.body_too_large", reBodyTooBig, nil, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.8,
			title: "Large uploads or requests are rejected by the server",
			what:  fmt.Sprintf("On %s, a request is refused because its body is bigger than the server allows (\"%s\").", c.page(), line),
			why:   "The server's request size limit is lower than what the app sends (often images or files sent as JSON or base64).",
			fix:   "Prefer uploading files directly to object storage from the browser (signed upload URLs) instead of through the server. Otherwise raise the limit for this route only (for example `express.json({ limit: '10mb' })`, `[RequestSizeLimit]` in .NET, `client_max_body_size` in nginx, `DATA_UPLOAD_MAX_MEMORY_SIZE` in Django), and check file size in the browser with a clear message.",
			check: fmt.Sprintf("Upload a large file on %s. It should succeed, or show a clear size message.", c.page())}
	}),
	mk("srv.csrf", reCSRF, nil, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.75,
			title: "Form submissions fail the security token check",
			what:  fmt.Sprintf("On %s, submitting a form is refused because its anti-forgery (CSRF) token is missing or doesn't match (\"%s\").", c.page(), line),
			why:   "The form doesn't include the token, the page was open so long that the session (and token) expired, cookies aren't sent (SameSite or a different domain), or the app runs on several servers that don't share the key.",
			fix:   "Make sure every form and AJAX request includes the CSRF token (for example `@csrf`/`{% csrf_token %}`/`asp-antiforgery`, or the token header for fetch calls). For long-open pages, show \"Your session expired, please reload\" instead of an error. If the app runs on several instances, share the data protection / session key between them.",
			check: fmt.Sprintf("Submit the form on %s normally, and after leaving the page open for a long time. Both should work or show a clear message.", c.page())}
	}),
	mk("srv.ef_tracking", reEFTracking, nil, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.8,
			title: "Updating a record fails because Entity Framework is already tracking a copy of it",
			what:  fmt.Sprintf("On %s, saving fails with \"%s\".", c.page(), line),
			why:   "The same record was loaded (or attached) earlier in the same DbContext, and the code then attaches or updates another object with the same key, for example `_context.Update(modelFromForm)` after a lookup.",
			fix:   "In this action, update the entity that's already tracked instead of attaching a second one: load it with `FindAsync(id)`, copy the form values onto it (or `_context.Entry(entity).CurrentValues.SetValues(model)`), then `SaveChangesAsync()`. Use `AsNoTracking()` for lookups that are only read. Make sure the DbContext is scoped per request, not a singleton.",
			check: fmt.Sprintf("Edit and save a record on %s. It should save.", c.page())}
	}),
	mk("srv.concurrency", reConcurrency, nil, func(c *Context, line string) spec {
		return spec{sev: "low", conf: 0.7,
			title: "Saving fails because the record was changed in the meantime",
			what:  fmt.Sprintf("On %s, saving is refused because someone (or another request) changed or deleted the record after it was loaded (\"%s\").", c.page(), line),
			why:   "Optimistic concurrency protects against overwriting changes; the app doesn't handle the conflict.",
			fix:   "Catch this exception where the record is saved and show \"This item was changed by someone else. Reload to see the latest version\" (or reload and re-apply the change automatically when that's safe). If the record was deleted, return \"not found\". Don't remove the concurrency check.",
			check: fmt.Sprintf("Open the same record on %s in two tabs, save in both. The second should show a clear message.", c.page())}
	}),
	mk("srv.disk_full", reDiskFull, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.8,
			title: "The server or database is out of disk space",
			what:  fmt.Sprintf("On %s, writing fails because the disk is full (\"%s\").", c.page(), line),
			why:   "Logs, uploads, temporary files or database data filled the disk.",
			fix:   "This needs attention on the server: free space now (old logs, temp files, old backups), then stop it from coming back: rotate and cap logs, move uploads to object storage, clean temp files after use, and increase the disk if the database really needs it. Add a disk-usage alert.",
			check: "Check free disk space after the cleanup, then repeat the action. It should work."}
	}),
	mk("srv.port_in_use", rePortInUse, nil, func(c *Context, line string) spec {
		return spec{sev: "high", conf: 0.8,
			title: "The server can't start because its port is taken",
			what:  fmt.Sprintf("The server fails to start (\"%s\"), so the app may be down.", line),
			why:   "Another process (often an old copy of the same app) still uses the port, or the app ignores the port the host assigns.",
			fix:   "Make the app listen on the port the host provides (`process.env.PORT`, `ASPNETCORE_URLS`, `$PORT`) instead of a fixed one, and make sure only one instance starts per container (stop the old process before starting the new one).",
			check: "Restart the app. It should start once and answer requests."}
	}),
}

var catalogLast = []entry{
	mk("net.rate_limited", nil, status(429), func(c *Context, line string) spec {
		n := c.net()
		return spec{sev: "medium", conf: 0.65,
			title: fmt.Sprintf("Requests to %s are being rate limited", n.Host),
			what:  fmt.Sprintf("On %s, %s refuses requests because too many were sent in a short time.", c.page(), n.Host),
			why:   "The app sends more requests than the service allows: a loop, a request per keystroke or per list item, retries without delay, or a component that fetches on every render.",
			fix:   "Find what sends these requests. Send fewer: fetch once and cache, debounce search inputs, batch per-item requests, and fetch from effects with correct dependencies. When a 429 comes back, wait (respect the `Retry-After` header) before retrying, and show a gentle message.",
			check: fmt.Sprintf("Use %s normally with the network tab open. Requests to %s should be few, and none should return 429.", c.page(), n.Host)}
	}),
	mk("net.payload_too_large", nil, status(413), func(c *Context, line string) spec {
		n := c.net()
		return spec{sev: "medium", conf: 0.75,
			title: fmt.Sprintf("Uploads to %s are too large", n.Path),
			what:  fmt.Sprintf("On %s, sending data to `%s` fails because the request is bigger than the server accepts.", c.page(), n.Path),
			why:   "Files or images are sent in the request body (often base64 inside JSON), beyond the size limit.",
			fix:   "Upload files directly to storage instead of through this endpoint (for example Supabase Storage or a signed upload URL), compress images in the browser first, and check the size before sending with a clear message. Raise the server's limit only if this endpoint must accept large bodies.",
			check: fmt.Sprintf("On %s, upload a large file. It should succeed or show a clear size message.", c.page())}
	}),
	mk("net.gateway", nil, status(502, 503, 504), func(c *Context, line string) spec {
		n := c.net()
		return spec{sev: "high", conf: 0.55,
			title: fmt.Sprintf("%s is down or too slow to answer", n.Host),
			what:  fmt.Sprintf("On %s, requests to %s%s fail with HTTP %d: the service behind it is down, restarting or too slow.", c.page(), n.Host, n.Path, n.Status),
			why:   "The backend crashed or is restarting, a request takes longer than the gateway's time limit (slow query, slow external call), or the service is overloaded.",
			fix:   fmt.Sprintf("Check the logs of the service behind `%s` at the time of these errors: crashes, restarts, or requests longer than the timeout. Fix the slow or crashing part (pagination, indexes, moving long work to a background job). In the app, retry this request once after a short delay and show \"Please try again\" instead of breaking.", n.Path),
			check: fmt.Sprintf("Use %s several times. The request to %s should answer quickly with 200.", c.page(), n.Path)}
	}),
	mk("srv.outbound_timeout", reTimeout, isServer, func(c *Context, line string) spec {
		return spec{sev: "medium", conf: 0.6,
			title: "A call from the server to another service times out or is cut off",
			what:  fmt.Sprintf("On %s, the server waits for another service and gives up (\"%s\").", c.page(), line),
			why:   "The other service is slow or down, the address or port is wrong or blocked by a firewall, or the connection is reused after the other side closed it.",
			fix:   "Find which service this call goes to (the URL or host in the code at this point). Check that the address is right and reachable from production. Set an explicit, reasonable timeout, retry idempotent calls once with a short delay, and return a clear \"try again\" response instead of an error page. Move slow calls to a background job if the user doesn't need the result immediately.",
			check: fmt.Sprintf("Repeat the action on %s. It should complete or show a clear message within a few seconds.", c.page())}
	}),
}
