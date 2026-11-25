package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourname/fixassist/internal/ingest"
)

// serverFixture runs a captured server envelope through Prepare exactly as
// ingest stores it, then builds the issue context from the stored payload.
func serverFixture(t *testing.T, name string) (ingest.EventRow, *Context) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../ingest/testdata/server", name+".envelope"))
	if err != nil {
		t.Fatal(err)
	}
	env, err := ingest.ParseEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range env.Items {
		if it.Header.Type != "event" {
			continue
		}
		row, err := ingest.Prepare([]byte("k"), it.Payload, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		c := &Context{Kind: row.Kind, Route: row.Route, Agent: "cursor", Backend: "unknown", Users: 1, Events: 3, Attempt: 1, Title: "t"}
		c.AddSample(row.Payload)
		return row, c
	}
	t.Fatalf("%s: no event item", name)
	return ingest.EventRow{}, nil
}

// Real envelopes from the official Node (8.55.0, Express) and Python (2.19.2,
// SQLite) SDKs, plus a .NET (Entity Framework) event written by hand in the
// Sentry .NET format, since no .NET SDK runs here.
func TestServerFixturesExplained(t *testing.T) {
	cases := []struct {
		file, key, route string
		want             []string // all must appear in the fix prompt
	}{
		{"node_null_property", "srv.null_reference", "/orders/<id>", []string{"Node.js", "`total`", "server.js` line 18", "order.total"}},
		{"node_numeric_overflow", "srv.number_out_of_range", "/orders", []string{"numeric field overflow", "Prisma", "in `saveOrder`"}},
		{"node_unique_chain", "srv.unique_violation", "/signup", []string{"users_email_key", "in `insertUser`"}},
		{"python_unique_chain", "srv.unique_violation", "", []string{"Python", "UNIQUE constraint failed: users.email", "in `import_signup`"}},
		{"python_not_null", "srv.not_null", "", []string{"NOT NULL constraint failed", "in `create_user`"}},
		{"python_missing_table", "srv.missing_schema", "", []string{"`orders`", "migration", "in `monthly_report`"}},
		{"python_bad_format", "srv.bad_format", "", []string{"invalid literal for int()", "in `parse_quantity`"}},
		{"dotnet_ef_out_of_range", "srv.number_out_of_range", "/Products/Create", []string{
			".NET (C#) server", "7771.00", "Entity Framework", "decimal(18,2)",
			"`Controllers/ProductsController.cs` line 58 in `Create`: `await _context.SaveChangesAsync();`",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			row, c := serverFixture(t, tc.file)
			if row.Kind != ingest.KindServer {
				t.Fatalf("kind = %s, want server", row.Kind)
			}
			if !strings.HasPrefix(row.ReleaseID, ingest.ServerReleasePrefix) {
				t.Errorf("release = %q, want the server: prefix", row.ReleaseID)
			}
			if row.Route != tc.route {
				t.Errorf("route = %q, want %q", row.Route, tc.route)
			}
			s := Explain(c)
			if s.CatalogKey != tc.key {
				t.Errorf("catalog = %q, want %q\n%s", s.CatalogKey, tc.key, s.FixPrompt)
			}
			prompt := ForAgent("cursor", s)
			for _, w := range tc.want {
				if !strings.Contains(prompt, w) {
					t.Errorf("prompt missing %q:\n%s", w, prompt)
				}
			}
			if strings.Contains(prompt, "node_modules") || strings.Contains(prompt, "Microsoft.EntityFrameworkCore.") {
				t.Errorf("prompt points at library code:\n%s", prompt)
			}
			if tc.route == "" && !strings.Contains(s.WhatBroke, "a background job") {
				t.Errorf("a job outside a request should say \"a background job\": %s", s.WhatBroke)
			}
		})
	}
}

// What a server SDK may attach that must never be stored.
func TestServerFixturesScrubbed(t *testing.T) {
	files, _ := filepath.Glob("../ingest/testdata/server/*.envelope")
	if len(files) < 9 {
		t.Fatalf("found %d server fixtures", len(files))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".envelope")
		row, _ := serverFixture(t, name)
		p := string(row.Payload)
		var ev struct {
			Modules   any            `json:"modules"`
			Threads   any            `json:"threads"`
			Request   map[string]any `json:"request"`
			Exception struct {
				Values []struct {
					Stacktrace struct {
						Frames []map[string]any `json:"frames"`
					} `json:"stacktrace"`
				} `json:"values"`
			} `json:"exception"`
		}
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Modules != nil || ev.Threads != nil {
			t.Errorf("%s: modules or threads kept", name)
		}
		for _, k := range []string{"data", "env", "query_string", "cookies"} {
			if _, ok := ev.Request[k]; ok {
				t.Errorf("%s: request.%s kept", name, k)
			}
		}
		for _, x := range ev.Exception.Values {
			for _, f := range x.Stacktrace.Frames {
				if _, ok := f["vars"]; ok {
					t.Errorf("%s: frame local variables kept", name)
				}
			}
		}
		for _, bad := range []string{"secret-cookie", "someone@example.com", "first@example.com", "203.0.113.7"} {
			if strings.Contains(p, bad) {
				t.Errorf("%s: stored payload contains %s", name, bad)
			}
		}
	}
}

// Requests with different IDs and values, and the same error, group together.
func TestServerFixturesGroup(t *testing.T) {
	a, _ := serverFixture(t, "node_null_property")   // GET /orders/42
	b, _ := serverFixture(t, "node_null_property_2") // GET /orders/77
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("same bug, different fingerprints: %s %s", a.Fingerprint, b.Fingerprint)
	}
	c, _ := serverFixture(t, "node_unique_chain")
	if c.Fingerprint == a.Fingerprint {
		t.Error("different bugs share a fingerprint")
	}
}

// The catalog recognizes each database's wording, not only the ones above.
func TestServerCatalogDialects(t *testing.T) {
	cases := []struct{ platform, typ, msg, key string }{
		{"csharp", "SqlException", "String or binary data would be truncated in table 'Shop.dbo.Users', column 'Name'. Truncated value: 'x'.", "srv.text_too_long"},
		{"php", "QueryException", "SQLSTATE[22001]: String data, right truncation: 1406 Data too long for column 'name' at row 1", "srv.text_too_long"},
		{"java", "DataIntegrityViolationException", "ERROR: value too long for type character varying(20)", "srv.text_too_long"},
		{"php", "QueryException", "SQLSTATE[22003]: Numeric value out of range: 1264 Out of range value for column 'price' at row 1", "srv.number_out_of_range"},
		{"csharp", "SqlException", "Arithmetic overflow error converting numeric to data type numeric.", "srv.number_out_of_range"},
		{"csharp", "SqlException", "Violation of UNIQUE KEY constraint 'UQ_Users_Email'. Cannot insert duplicate key in object 'dbo.Users'.", "srv.unique_violation"},
		{"php", "QueryException", "SQLSTATE[23000]: Integrity constraint violation: 1062 Duplicate entry 'a' for key 'users.email'", "srv.unique_violation"},
		{"node", "PrismaClientKnownRequestError", "Invalid `prisma.user.create()` invocation: Unique constraint failed on the fields: (`email`) P2002", "srv.unique_violation"},
		{"node", "MongoServerError", "E11000 duplicate key error collection: shop.users index: email_1", "srv.unique_violation"},
		{"csharp", "SqlException", "Cannot insert the value NULL into column 'Email', table 'Shop.dbo.Users'; column does not allow nulls. INSERT fails.", "srv.not_null"},
		{"ruby", "ActiveRecord::NotNullViolation", "PG::NotNullViolation: ERROR:  null value in column \"email\" of relation \"users\" violates not-null constraint", "srv.not_null"},
		{"csharp", "SqlException", "The INSERT statement conflicted with the FOREIGN KEY constraint \"FK_Orders_Users\".", "srv.foreign_key"},
		{"python", "IntegrityError", "update or delete on table \"users\" violates foreign key constraint \"orders_user_id_fkey\" on table \"orders\"", "srv.foreign_key"},
		{"csharp", "SqlException", "Invalid object name 'dbo.Coupons'.", "srv.missing_schema"},
		{"ruby", "ActiveRecord::StatementInvalid", "PG::UndefinedColumn: ERROR:  column \"discount\" does not exist", "srv.missing_schema"},
		{"node", "Error", "connect ECONNREFUSED 127.0.0.1:5432", "srv.db_unreachable"},
		{"csharp", "SqlException", "A network-related or instance-specific error occurred while establishing a connection to SQL Server.", "srv.db_unreachable"},
		{"python", "OperationalError", "FATAL:  sorry, too many clients already", "srv.db_unreachable"},
		{"csharp", "NullReferenceException", "Object reference not set to an instance of an object.", "srv.null_reference"},
		{"java", "NullPointerException", "Cannot invoke \"String.length()\" because \"name\" is null", "srv.null_reference"},
		{"python", "AttributeError", "'NoneType' object has no attribute 'email'", "srv.null_reference"},
		{"ruby", "NoMethodError", "undefined method `email' for nil", "srv.null_reference"},
		{"php", "Error", "Call to a member function save() on null", "srv.null_reference"},
		{"go", "*errors.errorString", "runtime error: invalid memory address or nil pointer dereference", "srv.null_reference"},
		{"csharp", "FormatException", "Input string was not in a correct format.", "srv.bad_format"},
		{"java", "NumberFormatException", "For input string: \"12,5\"", "srv.bad_format"},
		{"go", "*strconv.NumError", "strconv.Atoi: parsing \"abc\": invalid syntax", ""},
	}
	for _, tc := range cases {
		c := &Context{Kind: "server", Route: "/x", Users: 1, Events: 1, Attempt: 1,
			Server: []ServerSample{{Platform: tc.platform, Chain: []ServerExc{{Type: tc.typ, Message: tc.msg}}}}}
		s := Explain(c)
		if s.CatalogKey != tc.key {
			t.Errorf("%s %q: catalog = %q, want %q", tc.platform, tc.msg, s.CatalogKey, tc.key)
		}
		if s.FixPrompt == "" || s.HowToCheck == "" || s.WhatBroke == "" {
			t.Errorf("%q: incomplete suggestion %+v", tc.msg, s)
		}
	}
}
