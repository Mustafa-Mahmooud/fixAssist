package ai

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/normalize"
)

// Server-side errors, from a Sentry server SDK in any language. The catalog
// below matches error messages from the common databases (Postgres, SQL
// Server, MySQL/MariaDB, SQLite, Oracle, MongoDB) and runtimes (Node,
// Python, .NET, Java, PHP, Ruby, Go), since the same mistake reads
// differently in each. Prompts name the language and, where it helps, the
// usual framework's way to fix it.

// ServerSample is one server exception chain, as the founder's server saw it.
type ServerSample struct {
	Platform    string
	Chain       []ServerExc // root cause first
	Function    string      // the app's own function nearest the crash
	Where       []StackRef  // the app's frames, crash site first
	Transaction string
}

// ServerExc is one exception in a chain.
type ServerExc struct{ Type, Message string }

// StackRef is one of the app's own stack frames.
type StackRef struct {
	Path     string
	Line     int
	Function string
	Code     string
}

// Root is the innermost cause.
func (s *ServerSample) Root() ServerExc { return s.Chain[0] }

// Outer is the exception the app saw.
func (s *ServerSample) Outer() ServerExc { return s.Chain[len(s.Chain)-1] }

func (c *Context) srv() *ServerSample {
	if len(c.Server) == 0 {
		return nil
	}
	return &c.Server[0]
}

// shortPath keeps the last 3 segments of an absolute path; relative paths
// (what most SDKs send) are kept as they are.
func shortPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || (len(p) > 2 && p[1] == ':') {
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) > 3 {
			parts = parts[len(parts)-3:]
		}
		return strings.Join(parts, "/")
	}
	return p
}

func serverSample(ev *ingest.Event) ServerSample {
	se := ingest.ServerError(ev)
	s := ServerSample{Platform: ev.Platform, Function: se.AppFunction(), Transaction: ev.Transaction}
	for _, x := range se.Chain {
		s.Chain = append(s.Chain, ServerExc{Type: normalize.ShortType(x.Type), Message: x.Message})
	}
	// The app's own frames, crash site first, from the innermost exception
	// that has any: in .NET or Java the root cause is often thrown deep in a
	// library (all framework frames) and the app's line is in the wrapper.
	// The source line comes from the raw frames (normalize doesn't keep it).
	for ci, x := range se.Chain {
		var raw []ingest.Frame
		if st := ev.Exception.Values[ci].Stacktrace; st != nil {
			raw = st.Frames
		}
		for i := len(x.Frames) - 1; i >= 0 && len(s.Where) < 5; i-- {
			f := x.Frames[i]
			if !normalize.ServerFrameInApp(f) {
				continue
			}
			ref := StackRef{Path: shortPath(f.Filename), Line: f.Lineno, Function: normalize.CleanFunction(f.Function)}
			if i < len(raw) {
				ref.Code = strings.TrimSpace(raw[i].ContextLine)
			}
			s.Where = append(s.Where, ref)
		}
		if len(s.Where) > 0 {
			break
		}
	}
	return s
}

// language is the platform's display name.
func language(platform string) string {
	switch platform {
	case "csharp":
		return ".NET (C#)"
	case "node":
		return "Node.js"
	case "python":
		return "Python"
	case "php":
		return "PHP"
	case "ruby":
		return "Ruby"
	case "java":
		return "Java"
	case "go":
		return "Go"
	case "elixir":
		return "Elixir"
	case "rust":
		return "Rust"
	}
	return platform
}

// serverName is "Python server", ".NET (C#) server", or "server".
func serverName(platform string) string {
	if l := language(platform); l != "" {
		return l + " server"
	}
	return "server"
}

// messages is every message in the chain, for matching.
func (s *ServerSample) messages() string {
	var b strings.Builder
	for _, x := range s.Chain {
		b.WriteString(x.Type + ": " + x.Message + "\n")
	}
	return b.String()
}

func serverMatch(re *regexp.Regexp) func(c *Context) bool {
	return func(c *Context) bool {
		s := c.srv()
		return s != nil && re.MatchString(s.messages())
	}
}

// cause is the most specific chain message matching re, else the root's.
func (s *ServerSample) cause(re *regexp.Regexp) ServerExc {
	for _, x := range s.Chain {
		if re.MatchString(x.Type + ": " + x.Message) {
			return x
		}
	}
	return s.Root()
}

func firstSub(s string, res ...*regexp.Regexp) string {
	for _, re := range res {
		if m := re.FindStringSubmatch(s); m != nil {
			return m[1]
		}
	}
	return ""
}

// stackLocations is the "where it happened" part of every server prompt.
func stackLocations(s *ServerSample) string {
	if s == nil || len(s.Where) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nWhere it happened (from the server's stack trace, crash site first):")
	for _, w := range s.Where {
		fmt.Fprintf(&b, "\n- `%s`", w.Path)
		if w.Line > 0 {
			fmt.Fprintf(&b, " line %d", w.Line)
		}
		if w.Function != "" {
			fmt.Fprintf(&b, " in `%s`", w.Function)
		}
		if w.Code != "" {
			fmt.Fprintf(&b, ": `%s`", strings.ReplaceAll(truncate(w.Code, 160), "`", "'"))
		}
	}
	return b.String()
}

func chainText(s *ServerSample) string {
	var b strings.Builder
	for i := len(s.Chain) - 1; i >= 0; i-- {
		x := s.Chain[i]
		if i == len(s.Chain)-1 {
			fmt.Fprintf(&b, "%s: %s", x.Type, x.Message)
		} else {
			fmt.Fprintf(&b, "\n  caused by %s: %s", x.Type, x.Message)
		}
	}
	return b.String()
}

// migrationHint is how the usual framework for this language changes a column.
func migrationHint(platform, what string) string {
	switch platform {
	case "csharp":
		return " If the app uses Entity Framework, change " + what + " on the model property (attributes or `OnModelCreating`) and add a migration."
	case "python":
		return " If the app uses Django, change " + what + " on the model field and run `makemigrations`; with SQLAlchemy, change the column type and add an Alembic migration."
	case "node":
		return " If the app uses Prisma, change " + what + " in `schema.prisma` and run a migration; with Sequelize, TypeORM or Knex, change it in a new migration."
	case "php":
		return " If the app uses Laravel, change " + what + " in a new migration with `->change()`."
	case "ruby":
		return " If the app uses Rails, change " + what + " with `change_column` in a new migration."
	case "java":
		return " If the app uses JPA/Hibernate, change " + what + " in the `@Column` annotation and migrate the database (Flyway or Liquibase)."
	case "go":
		return " Change " + what + " in a new database migration."
	}
	return " Change " + what + " with a database migration."
}

var (
	reNumOutOfRange = regexp.MustCompile(`(?i)parameter value '[^']*' is out of range|arithmetic overflow error|numeric field overflow|out of range value for column|numeric value out of range|NUMERIC_VALUE_OUT_OF_RANGE|value larger than specified precision|ORA-01438`)
	reTextTooLong   = regexp.MustCompile(`(?i)value too long for type|string or binary data would be truncated|data too long for column|value too large for column|ORA-12899|string data, right truncation`)
	reUnique        = regexp.MustCompile(`(?i)duplicate key|violates unique constraint|unique constraint failed|cannot insert duplicate key|violation of (?:unique key|primary key) constraint|duplicate entry|E11000|UniqueViolation|\bP2002\b|SQLITE_CONSTRAINT_UNIQUE|ORA-00001`)
	reNotNullSrv    = regexp.MustCompile(`(?i)violates not-null constraint|cannot insert the value null into column|column '[^']+' cannot be null|not null constraint failed|NotNullViolation|ORA-01400|\bP2011\b`)
	reForeignKey    = regexp.MustCompile(`(?i)violates foreign key constraint|conflicted with the (?:foreign key|reference) constraint|a foreign key constraint fails|foreign key constraint failed|ForeignKeyViolation|\bP2003\b|ORA-0229[12]`)
	reMissingSchema = regexp.MustCompile(`(?i)relation "[^"]+" does not exist|column "[^"]+"(?: of relation "[^"]+")? does not exist|invalid object name|invalid column name|table '[^']+' doesn't exist|unknown column|no such table|no such column|UndefinedTable|UndefinedColumn|ORA-00942|ORA-00904|\bP202[12]\b`)
	reDBUnreachable = regexp.MustCompile(`(?i)ECONNREFUSED|connection refused|could not connect to server|a network-related or instance-specific error|timeout expired|too many connections|remaining connection slots|connection pool|unable to connect to (?:the )?database|SQLSTATE\[HY000\] \[2002\]|communications link failure|server closed the connection unexpectedly|terminating connection due to administrator command|sorry, too many clients`)
	reNullRef       = regexp.MustCompile(`(?i)NullReferenceException|NullPointerException|object reference not set to an instance|'NoneType' object has no attribute|cannot read propert(?:y|ies) of (?:undefined|null)|undefined method .* for nil|call to a member function .* on null|attempt to read property .* on null|nil pointer dereference`)
	reBadFormat     = regexp.MustCompile(`(?i)input string was not in a correct format|FormatException|NumberFormatException|for input string:|invalid literal for int\(\)|could not convert string to (?:float|int)|invalid input syntax for type|is not a valid (?:integer|number|date)`)

	reQuotedValue = regexp.MustCompile(`'(-?[\d.,]+)'`)
	reColumnQ     = regexp.MustCompile(`(?i)column '([^']+)'`)
	reColumnDQ    = regexp.MustCompile(`(?i)column "([^"]+)"`)
	reTableQ      = regexp.MustCompile(`(?i)table '([^']+)'`)
	reTableDQ     = regexp.MustCompile(`(?i)(?:relation|table) "([^"]+)"`)
	reObjectName  = regexp.MustCompile(`(?i)invalid (?:object|column) name '([^']+)'`)
	reNoSuch      = regexp.MustCompile(`(?i)no such (?:table|column): ([\w.]+)`)
	reUnknownCol  = regexp.MustCompile(`(?i)unknown column '([^']+)'`)
	reConstraint  = regexp.MustCompile(`(?i)constraint ["'\[]([\w.]+)["'\]]`) // only a quoted name: "constraint failed" isn't one
	reNoneAttr    = regexp.MustCompile(`has no attribute '([^']+)'`)
	reReading     = regexp.MustCompile(`\(reading '([^']+)'\)`)
	reNilMethod   = regexp.MustCompile("undefined method [`']([^'`]+)['`] for nil")
	reNullMember  = regexp.MustCompile(`(?:member function|read property) "?(\w+)`)
)

func quote(s string) string {
	if s == "" {
		return ""
	}
	return "`" + s + "`"
}

func orWord(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return "`" + s + "`"
}

var serverCatalog = []entry{
	{
		key:   "srv.number_out_of_range",
		match: serverMatch(reNumOutOfRange),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reNumOutOfRange)
			value := firstSub(x.Message, reQuotedValue)
			col := firstSub(x.Message, reColumnQ, reColumnDQ)
			valueText := "a number"
			if value != "" {
				valueText = "the number " + value
			}
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("Saving fails on %s when a number is too big for the database", c.page()),
				WhatBroke:  fmt.Sprintf("On %s, saving fails because %s is too big for the database column it goes into. Nothing gets saved and the person sees an error page.", c.page(), valueText),
				Why:        fmt.Sprintf("A number column%s was created with too few digits (for example `decimal(5,2)` only allows up to 999.99), so larger real values are rejected.", map[bool]string{true: " (" + quote(col) + ")", false: ""}[col != ""]),
				FixPrompt:  fmt.Sprintf("My app's %s fails when saving on %s with \"%s: %s\". The database rejects %s because the column it is saved into allows too few digits.\n\nFind which model property or column receives this value (look at the code below and at what the form on %s submits). Make that column big enough for real values, for example `decimal(18,2)` for money or amounts.%s Also validate the value in the form so an out-of-range number shows a clear message instead of an error page. Don't change other columns.", serverName(s.Platform), c.page(), x.Type, x.Message, valueText, c.page(), migrationHint(s.Platform, "the column's precision")),
				HowToCheck: fmt.Sprintf("On %s, save a large value such as %s again. It should save without an error page.", c.page(), map[bool]string{true: value, false: "the one that failed"}[value != ""]),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "srv.text_too_long",
		match: serverMatch(reTextTooLong),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reTextTooLong)
			col := firstSub(x.Message, reColumnQ, reColumnDQ)
			table := firstSub(x.Message, reTableQ, reTableDQ)
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("Saving fails on %s when a text is longer than the database allows", c.page()),
				WhatBroke:  fmt.Sprintf("On %s, saving fails because a text value is longer than its database column allows. Nothing gets saved.", c.page()),
				Why:        fmt.Sprintf("A text column%s was created with a short length limit, which real data such as names, emails or descriptions goes over.", map[bool]string{true: " (" + quote(col) + ")", false: ""}[col != ""]),
				FixPrompt:  fmt.Sprintf("My app's %s fails when saving on %s with \"%s: %s\". A text value is longer than the database column allows.\n\nFind the column%s%s that the form on %s fills in and make it longer (or unlimited text) with a migration.%s Also add the same maximum length to the form's validation so people get a clear message. Don't change other columns.", serverName(s.Platform), c.page(), x.Type, x.Message, map[bool]string{true: " " + quote(col), false: ""}[col != ""], map[bool]string{true: " in table " + quote(table), false: ""}[table != ""], c.page(), migrationHint(s.Platform, "the column's length")),
				HowToCheck: fmt.Sprintf("On %s, submit a long value (for example 60 characters) in each text field. It should save without an error page.", c.page()),
				Confidence: 0.8,
			}
		},
	},
	{
		key:   "srv.unique_violation",
		match: serverMatch(reUnique),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reUnique)
			constraint := firstSub(x.Message, reConstraint)
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("%s fails when the same record is saved twice", capFirst(c.page())),
				WhatBroke:  fmt.Sprintf("On %s, saving fails because a record with the same unique value already exists (for example the same email or the same item twice). The person sees an error page instead of a helpful message.", c.page()),
				Why:        "The database correctly refuses duplicates, but the server doesn't check for them first or handle the refusal, so it crashes.",
				FixPrompt:  fmt.Sprintf("My app's %s fails on %s with \"%s: %s\": it tries to insert a record that already exists%s.\n\nIn the code below, before inserting, check whether the record already exists (or use an upsert if updating it is the right behaviour), and handle the duplicate case by returning a clear message such as \"This already exists\" instead of crashing. Also make sure a double click or retry can't submit twice. Don't remove the unique rule from the database.", serverName(s.Platform), c.page(), x.Type, x.Message, map[bool]string{true: " (constraint " + quote(constraint) + ")", false: ""}[constraint != ""]),
				HowToCheck: fmt.Sprintf("On %s, submit the same data twice. The second time you should see a clear message, not an error page.", c.page()),
				Confidence: 0.75,
			}
		},
	},
	{
		key:   "srv.not_null",
		match: serverMatch(reNotNullSrv),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reNotNullSrv)
			col := firstSub(x.Message, reColumnQ, reColumnDQ)
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("Saving fails on %s because a required value is missing", c.page()),
				WhatBroke:  fmt.Sprintf("On %s, saving fails because a required field%s is empty. Nothing gets saved.", c.page(), map[bool]string{true: " (" + quote(col) + ")", false: ""}[col != ""]),
				Why:        "The database requires this value, but the server sends nothing for it: the form may not include the field, it may be named differently, or the code doesn't set it.",
				FixPrompt:  fmt.Sprintf("My app's %s fails on %s with \"%s: %s\": the column %s is required but the code saves it empty.\n\nIn the code below, find where this record is created and make sure %s is always set: read it from the form (check the field name matches), give it a sensible default, or show a validation message when it's missing. Only make the column optional if it truly is optional. Don't change anything else.", serverName(s.Platform), c.page(), x.Type, x.Message, orWord(col, "named in the error"), orWord(col, "that column")),
				HowToCheck: fmt.Sprintf("On %s, submit the form, also with optional fields left empty. It should save, or show a clear validation message.", c.page()),
				Confidence: 0.75,
			}
		},
	},
	{
		key:   "srv.foreign_key",
		match: serverMatch(reForeignKey),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reForeignKey)
			deleting := regexp.MustCompile(`(?i)delete|REFERENCE constraint|ORA-02292|update or delete`).MatchString(x.Message)
			what := "it points to a related record that doesn't exist"
			fix := "make sure the related record exists and its ID is passed correctly (check the form sends the right ID, and that the record wasn't deleted). If the reference is optional, save it as empty instead of an invalid ID"
			if deleting {
				what = "other records still point to the one being deleted"
				fix = "decide what should happen to the records that point to it: delete them too, unlink them, or stop the delete and show a clear message. Implement that in the delete code (or as an `ON DELETE` rule in a migration)"
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("%s fails because of a link between records", capFirst(c.page())),
				WhatBroke:  fmt.Sprintf("On %s, the change can't be saved because %s.", c.page(), what),
				Why:        "The database protects links between records, and the server doesn't handle this case.",
				FixPrompt:  fmt.Sprintf("My app's %s fails on %s with \"%s: %s\": %s.\n\nIn the code below, %s. Return a clear message to the user instead of an error page. Don't remove the database rule.", serverName(s.Platform), c.page(), x.Type, x.Message, what, fix),
				HowToCheck: fmt.Sprintf("Repeat the action on %s. It should work, or show a clear message instead of an error page.", c.page()),
				Confidence: 0.7,
			}
		},
	},
	{
		key:   "srv.missing_schema",
		match: serverMatch(reMissingSchema),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reMissingSchema)
			name := firstSub(x.Message, reObjectName, reNoSuch, reUnknownCol, reTableDQ, reColumnDQ, reTableQ)
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      fmt.Sprintf("%s fails: the database is missing %s", capFirst(c.page()), orWord(name, "a table or column")),
				WhatBroke:  fmt.Sprintf("On %s, the server asks the database for %s, which doesn't exist there, so the request fails.", c.page(), orWord(name, "a table or column")),
				Why:        "The code expects a table or column that the live database doesn't have: usually a migration that ran locally but not in production, or a name that was changed in one place only.",
				FixPrompt:  fmt.Sprintf("My app's %s fails on %s with \"%s: %s\".\n\nThe code uses %s, but the production database doesn't have it. Check whether a migration that creates it exists and hasn't been applied in production (then make sure migrations run on deploy), or whether the name in the code doesn't match the database (then fix the name in one place). Don't create tables by hand outside the migrations.", serverName(s.Platform), c.page(), x.Type, x.Message, orWord(name, "a table or column named in the error")),
				HowToCheck: "After deploying, " + c.retry() + ". It should work without an error.",
				Confidence: 0.75,
			}
		},
	},
	{
		key:   "srv.db_unreachable",
		match: serverMatch(reDBUnreachable),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reDBUnreachable)
			return Suggestion{
				Severity:   c.severity("high"),
				Title:      "The server can't reach its database",
				WhatBroke:  fmt.Sprintf("On %s, the server couldn't connect to the database in time, so the request failed.", c.page()),
				Why:        "The database was unreachable, overloaded or out of connections: a wrong connection setting, the database server being down or asleep, or the app opening more connections than allowed.",
				FixPrompt:  fmt.Sprintf("My app's %s fails on %s with \"%s: %s\".\n\nCheck, in this order: the production connection string or database URL (host, port, password, SSL setting); that the database server is running and allows connections from the app's host; and that the code reuses one connection pool instead of opening a new connection per request (and closes connections it opens). Fix what's wrong, and make the page show a friendly \"try again\" message when the database is briefly unavailable.", serverName(s.Platform), c.page(), x.Type, x.Message),
				HowToCheck: capFirst(c.retry()) + " several times in a row. It should work every time.",
				Confidence: 0.6,
			}
		},
	},
	{
		key:   "srv.null_reference",
		match: serverMatch(reNullRef),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reNullRef)
			prop := firstSub(x.Message, reNoneAttr, reReading, reNilMethod, reNullMember)
			reading := "a value"
			if prop != "" {
				reading = "`" + prop + "`"
			}
			near := ""
			if s.Function != "" {
				near = fmt.Sprintf(" in `%s`", s.Function)
			}
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("%s fails when something it needs is empty", capFirst(c.page())),
				WhatBroke:  fmt.Sprintf("On %s, the server crashes with \"%s\" and the person sees an error page.", c.page(), x.Message),
				Why:        "The code uses something that is empty at that moment: usually a record that wasn't found, a missing form value, or a logged-out user.",
				FixPrompt:  fmt.Sprintf("My app's %s crashes on %s with \"%s: %s\"%s: it uses %s on something that is null or empty.\n\nIn the code below, find what is empty at the crash site and handle that case: if a record wasn't found return a \"not found\" response, if a form value is missing show a validation message, if the user isn't logged in redirect to login. Keep the change to that spot.", serverName(s.Platform), c.page(), x.Type, x.Message, near, reading),
				HowToCheck: fmt.Sprintf("On %s, try it with missing data, a record that doesn't exist and while logged out. It should never show an error page.", c.page()),
				Confidence: 0.6,
			}
		},
	},
	{
		key:   "srv.bad_format",
		match: serverMatch(reBadFormat),
		build: func(c *Context) Suggestion {
			s := c.srv()
			x := s.cause(reBadFormat)
			return Suggestion{
				Severity:   c.severity("medium"),
				Title:      fmt.Sprintf("%s fails when a value isn't in the expected format", capFirst(c.page())),
				WhatBroke:  fmt.Sprintf("On %s, the server crashes converting a value (for example a number, date or ID typed into a form or put in the URL) that isn't in the format it expects.", c.page()),
				Why:        "The code converts text to a number, date or ID without checking it first, so unexpected input such as an empty field, a comma instead of a dot, or letters crashes it.",
				FixPrompt:  fmt.Sprintf("My app's %s crashes on %s with \"%s: %s\".\n\nIn the code below, replace the direct conversion with a safe one (for example `TryParse`, `try/except`, or a validation library), and when the value is invalid return a clear validation message instead of crashing. If the form sends numbers with a comma as the decimal separator, accept that too. Keep the change to that spot.", serverName(s.Platform), c.page(), x.Type, x.Message),
				HowToCheck: fmt.Sprintf("On %s, submit an empty value, letters and a number with a comma. You should see a validation message, never an error page.", c.page()),
				Confidence: 0.65,
			}
		},
	},
}



// genericServer is the diagnose-and-fix prompt for server errors outside the catalog.
func genericServer(c *Context) Suggestion {
	s := c.srv()
	root, outer := s.Root(), s.Outer()
	sug := Suggestion{Origin: "agent", Severity: c.severity("medium"), Confidence: 0.45, Title: c.Title}
	sug.WhatBroke = fmt.Sprintf("On %s, the server fails with \"%s: %s\"", c.page(), root.Type, root.Message)
	if len(s.Chain) > 1 {
		sug.WhatBroke += fmt.Sprintf(" (reported as %s)", outer.Type)
	}
	sug.WhatBroke += " and the request doesn't complete."
	var ev strings.Builder
	for i, x := range c.Server {
		fmt.Fprintf(&ev, "Example %d: %s\n", i+1, chainText(&x))
	}
	sug.FixPrompt = fmt.Sprintf("My app's %s throws this error on %s (%s).\n\n%s\nFind the cause in the server code, starting at the first location below. First explain in one sentence what is wrong, then fix only that, with the smallest change that works. If the situation can still happen (bad input, missing data), return a clear message instead of an error page. Don't change unrelated code.",
		serverName(s.Platform), c.page(), plural(c.Events, "time", "times"), ev.String())
	sug.HowToCheck = fmt.Sprintf("Repeat what people did on %s. It should work, and no new \"%s\" error should be reported.", c.page(), root.Type)
	if sug.Title == "" {
		sug.Title = sug.WhatBroke
	}
	return sug
}

// retry says how to repeat what failed: open the page, or run the job.
func (c *Context) retry() string {
	if c.Route == "" && c.srv() != nil {
		return "run " + c.page() + " again"
	}
	return "open " + c.page() + " again"
}
