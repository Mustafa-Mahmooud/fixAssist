package ai

import (
	"regexp"
	"strings"
	"testing"

	"github.com/yourname/fixassist/internal/normalize"
	"github.com/yourname/fixassist/internal/normalize/bodyparse"
)

// Contexts built the way AddSample builds them from stored events.

func jsC(route, typ, msg string) *Context {
	j := JSSample{Type: typ, Message: msg}
	if m := reEngineIdent.FindStringSubmatch(msg); m != nil {
		j.Property = m[1] // as AddSample does
	}
	return &Context{Kind: "js", Route: route, Users: 2, Events: 5, Attempt: 1, JS: []JSSample{j}}
}

func netC(route, method, url string, status int, body, browserErr string) *Context {
	n := NetSample{Method: method, Path: normalize.RouteTemplate(url), Host: hostOf(url), Status: status, Error: browserErr, Body: body}
	if body != "" {
		res := bodyparse.Parse(bodyparse.Input{Status: status, URL: url, Body: []byte(body)})
		n.Backend, n.Code, n.Message = res.Backend, res.Code, res.Message
		n.Details, n.Hint = postgrestExtras(body)
	}
	return &Context{Kind: "net", Route: route, Users: 2, Events: 5, Attempt: 1, Net: []NetSample{n}}
}

func srvC(route, platform string, chain ...string) *Context {
	s := ServerSample{Platform: platform}
	for _, x := range chain {
		typ, msg, _ := strings.Cut(x, ": ")
		s.Chain = append(s.Chain, ServerExc{Type: typ, Message: msg})
	}
	return &Context{Kind: "server", Route: route, Users: 1, Events: 3, Attempt: 1, Server: []ServerSample{s}}
}

type catalogCase struct {
	name string
	c    *Context
	key  string
	want []string // in the fix prompt or the explanation
}

// catalogMoreCases covers every extended catalog entry. Also the bootstrap
// set for fine-tuning (TestExportCatalogBootstrap).
func catalogMoreCases() []catalogCase {
	const sb = "https://abc.supabase.co"
	return []catalogCase{
		// Noise.
		{"abort", jsC("/search", "AbortError", "signal is aborted without reason"), "noise.aborted", []string{"AbortError"}},
		{"resize observer", jsC("/", "Error", "ResizeObserver loop completed with undelivered notifications."), "noise.resize_observer", []string{"requestAnimationFrame"}},
		{"script error", jsC("/", "", "Script error."), "noise.script_error", []string{"crossorigin", `"Script error."`}},

		// Supabase setup and Edge Functions.
		{"supabase env", jsC("/", "Error", "supabaseUrl is required."), "sb.env_missing", []string{"VITE_SUPABASE_URL", "service-role"}},
		{"supabase bad key", netC("/", "GET", sb+"/rest/v1/todos", 401, `{"message":"Invalid API key","hint":"Double check your Supabase anon or service_role API key."}`, ""), "sb.bad_key", []string{"anon", "Invalid API key"}},
		{"edge cors", netC("/invoices", "POST", sb+"/functions/v1/send-email", 0, "", "Failed to fetch"), "sb.edge_cors", []string{"`send-email`", "OPTIONS", "supabase functions deploy send-email"}},
		{"edge fetch error js", jsC("/invoices", "FunctionsFetchError", "Failed to send a request to the Edge Function"), "sb.edge_cors", []string{"OPTIONS", "Access-Control-Allow-Headers"}},
		{"edge missing", netC("/invoices", "POST", sb+"/functions/v1/send-email", 404, `{"code":"NOT_FOUND","message":"Requested function was not found"}`, ""), "sb.edge_missing", []string{"supabase/functions/send-email"}},
		{"edge 500", netC("/invoices", "POST", sb+"/functions/v1/send-invoice", 500, `{"error":"RESEND_API_KEY is not set"}`, ""), "sb.edge_error", []string{"`send-invoice`", "supabase secrets set", "RESEND_API_KEY is not set"}},
		{"edge http js", jsC("/invoices", "FunctionsHttpError", "Edge Function returned a non-2xx status code"), "sb.edge_error", []string{"Edge Functions > Logs"}},

		// Third-party APIs.
		{"openai key in browser", netC("/chat", "POST", "https://api.openai.com/v1/chat/completions", 401, `{"error":{"message":"Incorrect API key provided: sk-abc. You can find your API key at https://platform.openai.com/account/api-keys.","type":"invalid_request_error","code":"invalid_api_key"}}`, ""), "api.key_invalid", []string{"Incorrect API key", "anyone can copy it", "rotate"}},
		{"stripe key server", srvC("/checkout", "node", "StripeAuthenticationError: Invalid API Key provided: sk_live_****abcd"), "api.key_invalid", []string{"Node.js server", "Never hard-code"}},
		{"openai quota server", srvC("/chat", "python", "RateLimitError: Error code: 429 - {'error': {'message': 'You exceeded your current quota, please check your plan and billing details.', 'type': 'insufficient_quota'}}"), "api.quota", []string{"billing", "temporarily unavailable"}},
		{"openai quota beats 429", netC("/chat", "POST", "https://api.openai.com/v1/chat/completions", 429, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, ""), "api.quota", []string{"server function"}},
		{"stripe test mode", srvC("/checkout", "node", "StripeInvalidRequestError: No such price: 'price_1Abc'; a similar object exists in test mode, but a live mode key was used to make this request."), "api.stripe_mode", []string{"live-mode ID", "No such price"}},
		{"stripe webhook", srvC("/api/stripe/webhook", "node", "StripeSignatureVerificationError: No signatures found matching the expected signature for payload. Are you passing the raw request body you received from Stripe?"), "api.stripe_webhook", []string{"raw request body", "whsec_"}},

		// PostgREST.
		{"column missing", netC("/orders/new", "POST", sb+"/rest/v1/orders", 400, `{"code":"PGRST204","details":null,"hint":null,"message":"Could not find the 'createdAt' column of 'orders' in the schema cache"}`, ""), "pg.column_missing", []string{"`createdAt`", "`orders`", "migration"}},
		{"function missing", netC("/stats", "POST", sb+"/rest/v1/rpc/get_stats", 404, `{"code":"PGRST202","details":"Searched for the function public.get_stats with parameter user_id","hint":"Perhaps you meant to call the function public.get_stats(uid)","message":"Could not find the function public.get_stats(user_id) in the schema cache"}`, ""), "pg.function_missing", []string{"supabase.rpc('get_stats'", "`user_id`", "reload schema"}},
		{"relationship missing", netC("/feed", "GET", sb+"/rest/v1/posts?select=*,profiles(*)", 400, `{"code":"PGRST200","details":"Searched for a foreign key relationship between 'posts' and 'profiles' in the schema 'public', but no matches were found.","hint":null,"message":"Could not find a relationship between 'posts' and 'profiles' in the schema cache"}`, ""), "pg.relationship_missing", []string{"foreign key", "`posts`", "`profiles`"}},
		{"relationship ambiguous", netC("/feed", "GET", sb+"/rest/v1/posts", 300, `{"code":"PGRST201","details":[],"hint":"Try changing 'profiles' to one of the following...","message":"Could not embed because more than one relationship was found for 'posts' and 'profiles'"}`, ""), "pg.relationship_missing", []string{"!posts_author_id_fkey"}},
		{"jwt expired", netC("/dashboard", "GET", sb+"/rest/v1/projects", 401, `{"code":"PGRST301","details":null,"hint":null,"message":"JWT expired"}`, ""), "pg.jwt_expired", []string{"autoRefreshToken", "onAuthStateChange"}},
		{"check violation", netC("/orders/new", "POST", sb+"/rest/v1/orders", 400, `{"code":"23514","details":"Failing row contains (...)","hint":null,"message":"new row for relation \"orders\" violates check constraint \"orders_status_check\""}`, ""), "pg.check_violation", []string{"`orders_status_check`"}},
		{"statement timeout", netC("/reports", "GET", sb+"/rest/v1/events", 500, `{"code":"57014","details":null,"hint":null,"message":"canceling statement due to statement timeout"}`, ""), "pg.timeout", []string{".limit()", "index"}},
		{"range", netC("/list", "GET", sb+"/rest/v1/items", 416, `{"code":"PGRST103","details":"An offset of 40 was requested, but there are only 12 rows.","hint":null,"message":"Requested range not satisfiable"}`, ""), "pg.range", []string{".range(from, to)"}},

		// Storage.
		{"bucket missing", netC("/profile", "POST", sb+"/storage/v1/object/avatars/u1/a.png", 400, `{"statusCode":"404","error":"Bucket not found","message":"Bucket not found"}`, ""), "sb.storage_bucket", []string{"`avatars`", "storage.buckets"}},
		{"storage rls", netC("/profile", "POST", sb+"/storage/v1/object/avatars/u1/a.png", 403, `{"statusCode":"403","error":"Unauthorized","message":"new row violates row-level security policy"}`, ""), "sb.storage_rls", []string{"storage.objects", "auth.uid()", "public"}},
		{"storage exists", netC("/profile", "POST", sb+"/storage/v1/object/avatars/u1/a.png", 409, `{"statusCode":"409","error":"Duplicate","message":"The resource already exists"}`, ""), "sb.storage_exists", []string{"upsert: true"}},
		{"storage too large", netC("/profile", "POST", sb+"/storage/v1/object/docs/u1/big.pdf", 413, `{"statusCode":"413","error":"Payload too large","message":"The object exceeded the maximum allowed size"}`, ""), "sb.storage_too_large", []string{"resumable"}},

		// Auth.
		{"refresh token (code)", netC("/", "POST", sb+"/auth/v1/token?grant_type=refresh_token", 400, `{"code":"refresh_token_not_found","message":"Invalid Refresh Token: Refresh Token Not Found"}`, ""), "auth.refresh_token", []string{"signOut({ scope: 'local' })"}},
		{"refresh token (invalid_grant)", netC("/", "POST", sb+"/auth/v1/token?grant_type=refresh_token", 400, `{"error":"invalid_grant","error_description":"Invalid Refresh Token: Refresh Token Not Found"}`, ""), "auth.refresh_token", []string{"exactly one Supabase client"}},
		{"wrong password still", netC("/login", "POST", sb+"/auth/v1/token?grant_type=password", 400, `{"code":"invalid_credentials","message":"Invalid login credentials"}`, ""), "auth.invalid_credentials", nil},
		{"link expired", netC("/auth/callback", "GET", sb+"/auth/v1/verify", 403, `{"code":"otp_expired","message":"Email link is invalid or has expired"}`, ""), "auth.link_expired", []string{"Site URL", "redirect URLs"}},
		{"signup disabled", netC("/signup", "POST", sb+"/auth/v1/signup", 422, `{"code":"signup_disabled","message":"Signups not allowed for this instance"}`, ""), "auth.config", []string{"Authentication > Providers"}},
		{"email not authorized", netC("/signup", "POST", sb+"/auth/v1/signup", 400, `{"code":"email_address_not_authorized","message":"Email address \"a@b.co\" cannot be used as it is not authorized"}`, ""), "auth.invalid_email", []string{"SMTP"}},
		{"same password", netC("/account", "PUT", sb+"/auth/v1/user", 422, `{"code":"same_password","message":"New password should be different from the old password."}`, ""), "auth.same_password", []string{"must be different"}},
		{"auth rate limit beats 429", netC("/login", "POST", sb+"/auth/v1/token?grant_type=password", 429, `{"code":"over_request_rate_limit","message":"Request rate limit reached"}`, ""), "auth.rate_limit", []string{"disable the button"}},
		{"email rate limit still", netC("/signup", "POST", sb+"/auth/v1/signup", 429, `{"code":"over_email_send_rate_limit","message":"email rate limit exceeded"}`, ""), "auth.email_rate_limit", nil},

		// Firebase.
		{"firebase env", jsC("/", "FirebaseError", "Firebase: Error (auth/invalid-api-key)."), "fb.env_missing", []string{"initializeApp"}},
		{"firestore rules", jsC("/notes", "FirebaseError", "Missing or insufficient permissions."), "fb.permission_denied", []string{"firestore.rules", "onAuthStateChanged"}},
		{"firestore index", jsC("/notes", "FirebaseError", "The query requires an index. You can create it here: https://console.firebase.google.com/v1/r/project/p1/firestore/indexes?create_composite=Cklw"), "fb.index_required", []string{"https://console.firebase.google.com/v1/r/project/p1/firestore/indexes?create_composite=Cklw", "firestore.indexes.json"}},
		{"firebase credential js", jsC("/login", "FirebaseError", "Firebase: Error (auth/invalid-credential)."), "fb.auth_credentials", []string{"Email or password is incorrect"}},
		{"firebase credential net", netC("/login", "POST", "https://identitytoolkit.googleapis.com/v1/accounts:signInWithPassword", 400, `{"error":{"code":400,"message":"INVALID_LOGIN_CREDENTIALS","status":"INVALID_ARGUMENT"}}`, ""), "fb.auth_credentials", nil},
		{"firebase email in use", jsC("/signup", "FirebaseError", "Firebase: Error (auth/email-already-in-use)."), "fb.auth_email_in_use", []string{"already has an account"}},
		{"firebase weak", jsC("/signup", "FirebaseError", "Firebase: Password should be at least 6 characters (auth/weak-password)."), "fb.auth_weak_password", nil},
		{"firebase domain", jsC("/login", "FirebaseError", "Firebase: Error (auth/unauthorized-domain)."), "fb.unauthorized_domain", []string{"Authorized domains"}},
		{"firebase popup", jsC("/login", "FirebaseError", "Firebase: Error (auth/popup-closed-by-user)."), "fb.popup", []string{"signInWithRedirect"}},

		// React and the browser.
		{"hydration minified", jsC("/", "Error", "Minified React error #418; visit https://react.dev/errors/418 for the full message"), "js.hydration", []string{"useEffect", "<p>"}},
		{"hydration text", jsC("/", "Error", "Hydration failed because the initial UI does not match what was rendered on the server."), "js.hydration", nil},
		{"render loop", jsC("/cart", "Error", "Too many re-renders. React limits the number of renders to prevent an infinite loop."), "js.render_loop", []string{"onClick={() => handler()}"}},
		{"update depth", jsC("/cart", "Error", "Maximum update depth exceeded. This can happen when a component calls setState inside useEffect"), "js.render_loop", nil},
		{"hooks order", jsC("/cart", "Error", "Rendered more hooks than during the previous render."), "js.hooks_order", []string{"before any `if`"}},
		{"invalid hook call", jsC("/", "Error", "Invalid hook call. Hooks can only be called inside of the body of a function component."), "js.hooks_order", []string{"npm ls react"}},
		{"no router", jsC("/settings", "Error", "useNavigate() may be used only in the context of a <Router> component."), "js.no_provider", []string{"`useNavigate`", "`Router`"}},
		{"no auth provider", jsC("/settings", "Error", "useAuth must be used within an AuthProvider"), "js.no_provider", []string{"`useAuth`", "`AuthProvider`"}},
		{"no query client", jsC("/settings", "Error", "No QueryClient set, use QueryClientProvider to set one"), "js.no_provider", nil},
		{"object child", jsC("/profile", "Error", "Objects are not valid as a React child (found: object with keys {id, name})."), "js.object_child", []string{".name"}},
		{"invalid date", jsC("/orders", "RangeError", "Invalid time value"), "js.invalid_date", []string{"isValid"}},
		{"stack overflow js", jsC("/tree", "RangeError", "Maximum call stack size exceeded"), "js.stack_overflow", nil},
		{"stack overflow java", srvC("/tree", "java", "StackOverflowError: "), "js.stack_overflow", nil},
		{"storage quota", jsC("/cart", "QuotaExceededError", "Failed to execute 'setItem' on 'Storage': Setting the value of 'cart' exceeded the quota."), "js.storage_quota", []string{"IndexedDB"}},
		{"null property still", jsC("/orders", "TypeError", "Cannot read properties of null (reading 'total')"), "js.null_property", nil},

		// Servers.
		{"smtp dotnet", srvC("/Account/Register", "csharp", "IOException: Unable to read data from the transport connection: The connection was closed.", "SmtpException: Failure sending mail."), "srv.email_send", []string{"port 587", "MailKit"}},
		{"smtp python", srvC("/signup", "python", "SMTPAuthenticationError: (535, b'5.7.8 Username and Password not accepted')"), "srv.email_send", []string{"app password"}},
		{"smtp nodemailer", srvC("/signup", "node", "Error: Invalid login: 535-5.7.8 Username and Password not accepted"), "srv.email_send", nil},
		{"smtp refused beats db", srvC("/signup", "node", "Error: connect ECONNREFUSED 52.1.2.3:587"), "srv.email_send", nil},
		{"db port timeout", srvC("/", "node", "Error: connect ETIMEDOUT 10.0.0.5:5432"), "srv.db_unreachable", nil},
		{"db auth pg", srvC("/", "node", `error: password authentication failed for user "postgres"`), "srv.db_auth", []string{"URL-encoded"}},
		{"db auth mssql", srvC("/", "csharp", "SqlException: Login failed for user 'app'."), "srv.db_auth", nil},
		{"db auth mysql", srvC("/", "php", "PDOException: SQLSTATE[HY000] [1045] Access denied for user 'root'@'10.0.0.1' (using password: YES)"), "srv.db_auth", nil},
		{"tls expired", srvC("/sync", "node", "Error: certificate has expired"), "srv.tls", []string{"Don't disable certificate verification"}},
		{"tls java", srvC("/sync", "java", "SSLHandshakeException: PKIX path building failed: unable to find valid certification path"), "srv.tls", nil},
		{"missing env", srvC("/", "node", "Error: Missing required environment variable: DATABASE_URL"), "srv.missing_config", []string{"production environment"}},
		{"missing env python", srvC("/", "python", "KeyError: 'STRIPE_SECRET_KEY'"), "srv.missing_config", []string{"STRIPE_SECRET_KEY"}},
		{"missing conn string", srvC("/", "csharp", "InvalidOperationException: The ConnectionString property has not been initialized."), "srv.missing_config", nil},
		{"jwt secret missing", srvC("/login", "node", "Error: secretOrPrivateKey must have a value"), "srv.missing_config", nil},
		{"jwt expired server", srvC("/api/me", "node", "TokenExpiredError: jwt expired"), "srv.jwt", []string{"401"}},
		{"jwt signature", srvC("/api/me", "csharp", "SecurityTokenInvalidSignatureException: IDX10503: Signature validation failed."), "srv.jwt", []string{"same secret"}},
		{"deadlock pg", srvC("/checkout", "python", "OperationalError: deadlock detected"), "srv.deadlock", []string{"retry"}},
		{"deadlock mssql", srvC("/checkout", "csharp", "SqlException: Transaction (Process ID 61) was deadlocked on lock resources with another process and has been chosen as the deadlock victim."), "srv.deadlock", nil},
		{"linq empty", srvC("/Orders/Details", "csharp", "InvalidOperationException: Sequence contains no elements"), "srv.not_found", []string{"FirstOrDefault", "404"}},
		{"linq many", srvC("/Orders/Details", "csharp", "InvalidOperationException: Sequence contains more than one element"), "srv.not_found", []string{"unique constraint"}},
		{"django not found", srvC("/products/<id>", "python", "DoesNotExist: Product matching query does not exist."), "srv.not_found", nil},
		{"laravel not found", srvC("/orders/<id>", "php", `ModelNotFoundException: No query results for model [App\Models\Order] 42`), "srv.not_found", nil},
		{"sql null", srvC("/Products", "csharp", "SqlNullValueException: Data is Null. This method or property cannot be called on Null values."), "srv.sql_null", []string{"string?"}},
		{"dotnet missing assembly", srvC("/", "csharp", "FileNotFoundException: Could not load file or assembly 'Newtonsoft.Json, Version=13.0.0.0' or one of its dependencies."), "srv.module_missing", []string{"`Newtonsoft.Json`"}},
		{"node missing module", srvC("/", "node", "Error: Cannot find module 'bcrypt'"), "srv.module_missing", []string{"`bcrypt`", "dependencies"}},
		{"python missing module", srvC("/", "python", "ModuleNotFoundError: No module named 'stripe'"), "srv.module_missing", []string{"`stripe`"}},
		{"file missing", srvC("/invoice", "node", "Error: ENOENT: no such file or directory, open '/app/templates/invoice.html'"), "srv.file_missing", []string{"__dirname"}},
		{"read-only fs", srvC("/upload", "node", "Error: EROFS: read-only file system, open '/var/task/uploads/a.png'"), "srv.readonly_fs", []string{"object storage"}},
		{"oom node", srvC("/export", "node", "Error: FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory"), "srv.out_of_memory", []string{"pagination"}},
		{"oom php", srvC("/export", "php", "FatalError: Allowed memory size of 134217728 bytes exhausted (tried to allocate 20480 bytes)"), "srv.out_of_memory", nil},
		{"bad json node", srvC("/api/orders", "node", "SyntaxError: Unexpected token o in JSON at position 1"), "srv.bad_json", []string{"400"}},
		{"bad json python", srvC("/api/orders", "python", "JSONDecodeError: Expecting value: line 1 column 1 (char 0)"), "srv.bad_json", nil},
		{"body too large", srvC("/api/upload", "node", "PayloadTooLargeError: request entity too large"), "srv.body_too_large", []string{"express.json({ limit"}},
		{"csrf laravel", srvC("/profile", "php", "TokenMismatchException: CSRF token mismatch."), "srv.csrf", []string{"@csrf"}},
		{"csrf dotnet", srvC("/Account/Login", "csharp", "AntiforgeryValidationException: The required antiforgery cookie is not present."), "srv.csrf", nil},
		{"ef tracking", srvC("/Products/Edit", "csharp", "InvalidOperationException: The instance of entity type 'Product' cannot be tracked because another instance with the same key value for {'Id'} is already being tracked."), "srv.ef_tracking", []string{"SetValues", "AsNoTracking"}},
		{"concurrency", srvC("/Products/Edit", "csharp", "DbUpdateConcurrencyException: The database operation was expected to affect 1 row(s), but actually affected 0 row(s)"), "srv.concurrency", []string{"changed by someone else"}},
		{"disk full", srvC("/upload", "node", "Error: ENOSPC: no space left on device, write"), "srv.disk_full", []string{"rotate"}},
		{"port in use", srvC("", "node", "Error: listen EADDRINUSE: address already in use :::3000"), "srv.port_in_use", []string{"process.env.PORT"}},
		{"outbound timeout", srvC("/sync", "node", "Error: connect ETIMEDOUT 52.1.2.3:443"), "srv.outbound_timeout", []string{"timeout"}},
		{"httpclient timeout", srvC("/sync", "csharp", "TaskCanceledException: The request was canceled due to the configured HttpClient.Timeout of 100 seconds elapsing."), "srv.outbound_timeout", nil},
		{"null ref still", srvC("/", "csharp", "NullReferenceException: Object reference not set to an instance of an object."), "srv.null_reference", nil},
		{"db unreachable still", srvC("/", "node", "Error: connect ECONNREFUSED 127.0.0.1:5432"), "srv.db_unreachable", nil},

		// Broad network entries.
		{"rate limited", netC("/search", "GET", "https://api.example.com/search?q=a", 429, `{"message":"Too Many Requests"}`, ""), "net.rate_limited", []string{"debounce", "Retry-After"}},
		{"payload too large", netC("/upload", "POST", "https://app.example.com/api/upload", 413, "", ""), "net.payload_too_large", nil},
		{"gateway", netC("/reports", "GET", "https://app.example.com/api/report", 504, "<html>504 Gateway Time-out</html>", ""), "net.gateway", []string{"/api/report"}},
		{"local address still", netC("/", "GET", "http://localhost:3000/api/x", 0, "", "Failed to fetch"), "net.local_address", nil},
	}
}

func TestCatalogMore(t *testing.T) {
	cases := catalogMoreCases()
	seen := map[string]bool{}
	// An empty quote (not a quoted message that itself ends in a quote).
	reBroken := regexp.MustCompile(`%!|\(MISSING\)|(?:^|[\s(:])""|<nil>`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Explain(tc.c)
			if s.CatalogKey != tc.key {
				t.Fatalf("catalog = %q, want %q\n%s", s.CatalogKey, tc.key, s.FixPrompt)
			}
			seen[s.CatalogKey] = true
			if s.Title == "" || s.WhatBroke == "" || s.FixPrompt == "" || s.HowToCheck == "" || s.Severity == "" || s.Confidence <= 0 {
				t.Errorf("incomplete suggestion: %+v", s)
			}
			all := s.Title + "\n" + s.WhatBroke + "\n" + s.Why + "\n" + s.FixPrompt + "\n" + s.HowToCheck
			if m := reBroken.FindString(all); m != "" {
				t.Errorf("broken text %q in:\n%s", m, all)
			}
			for _, w := range tc.want {
				if !strings.Contains(all, w) {
					t.Errorf("missing %q in:\n%s", w, all)
				}
			}
		})
	}
	// Every entry of the extended catalog is exercised by at least one case.
	for _, group := range [][]entry{catalogFirst, browserMore, serverFirst, catalogLast} {
		for _, e := range group {
			if !seen[e.key] {
				t.Errorf("catalog entry %s has no test case", e.key)
			}
		}
	}
}

// Keys are unique: the knowledge base tracks results per key.
func TestCatalogKeysUnique(t *testing.T) {
	keys := map[string]bool{}
	for _, e := range catalog {
		if keys[e.key] {
			t.Errorf("duplicate catalog key %s", e.key)
		}
		keys[e.key] = true
	}
	if len(catalog) < 90 {
		t.Errorf("catalog has %d entries", len(catalog))
	}
}
