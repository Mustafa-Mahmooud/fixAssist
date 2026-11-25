package normalize

import (
	"regexp"
	"strings"
)

// Server-side errors come from Sentry server SDKs in any language (Node,
// Python, PHP, Ruby, Java, .NET, Go, ...). Their fingerprint uses signals
// that stay stable across deploys and across the values in one request:
//
//  - the exception chain's types, root cause first (a .NET DbUpdateException
//    wrapping an ArgumentException is a different bug from a bare
//    DbUpdateException);
//  - the root cause's normalized message (quoted values and long numbers
//    removed, so '7771.00' and '9000.50' group together);
//  - the app's own function nearest to the crash (module + function, never
//    line numbers, which change with every edit);
//  - the route the request was for.
//
// Fingerprints carry "v2|server|", separate from browser errors.

// ServerFrame is one stack frame from a server SDK.
type ServerFrame struct {
	Module   string // namespace / module / class path
	Function string
	Filename string
	Lineno   int
	InApp    *bool
}

// ServerException is one exception in a chain.
type ServerException struct {
	Type    string
	Message string
	Frames  []ServerFrame // oldest call first, crash site last (Sentry order)
}

// ServerError is a server-side exception with its chain.
type ServerError struct {
	Platform    string            // csharp, python, node, php, ruby, java, go, ...
	Chain       []ServerException // root cause first, outermost last (Sentry order)
	URL         string            // request URL, when the error happened in a request
	Transaction string            // the SDK's name for the request or job
}

// Root is the innermost cause: the most specific message.
func (e ServerError) Root() ServerException {
	if len(e.Chain) == 0 {
		return ServerException{}
	}
	return e.Chain[0]
}

// Outer is the exception the app actually saw.
func (e ServerError) Outer() ServerException {
	if len(e.Chain) == 0 {
		return ServerException{}
	}
	return e.Chain[len(e.Chain)-1]
}

// Types is the chain's types, root cause first: "ArgumentException<DbUpdateException".
func (e ServerError) Types() string {
	ts := make([]string, 0, len(e.Chain))
	for _, x := range e.Chain {
		ts = append(ts, ShortType(x.Type))
	}
	return strings.Join(ts, "<")
}

// ShortType drops a namespace: "System.ArgumentException" → "ArgumentException",
// "psycopg2.errors.UniqueViolation" → "UniqueViolation".
func ShortType(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.LastIndexAny(t, ".\\:"); i >= 0 && i < len(t)-1 {
		return t[i+1:]
	}
	return t
}

// Library code, for SDKs that don't mark frames in_app. Matched against
// module and filename.
var reLibraryFrame = regexp.MustCompile(`(?i)(^|[/\\.])(node_modules|site-packages|dist-packages|vendor|gems|bundle|lib/python|usr/lib|usr/local/lib|\.cargo|go/pkg/mod)([/\\.]|$)|^(System|Microsoft|java|javax|jdk|sun|kotlin|scala|org\.springframework|org\.hibernate|org\.apache|com\.fasterxml|io\.netty|Illuminate|Symfony|Laravel|django|flask|werkzeug|starlette|fastapi|sqlalchemy|express|internal|node:|runtime|net/http|database/sql|ActiveRecord|ActionController|ActionDispatch|Rack|Npgsql|MySql|Pomelo|Dapper)([./\\:]|$)`)

// frameInApp: library paths win over the SDK's flag (the Node SDK on Windows
// marks node_modules frames in_app), then the flag, else in-app.
func frameInApp(f ServerFrame) bool {
	if reLibraryFrame.MatchString(f.Module) || reLibraryFrame.MatchString(f.Filename) {
		return false
	}
	if f.InApp != nil {
		return *f.InApp
	}
	return true
}

var (
	reAsyncName = regexp.MustCompile(`<([A-Za-z_][\w]*)>[a-z]__\w*`) // .NET async state machines: <Create>d__5
	reLambda    = regexp.MustCompile(`<(?:lambda|listcomp|genexpr|dictcomp|setcomp)>|<>c(?:__DisplayClass\w*)?|\$\d+|\bb__\d+_\d+`)
)

// CleanFunction removes parameters, generics and compiler-generated parts so
// the same method keeps the same name: ".<Create>d__5.MoveNext()" → "Create".
func CleanFunction(fn string) string {
	fn = strings.TrimSpace(fn)
	switch fn {
	case "?", "<anonymous>", "<unknown>", "anonymous", "{anonymous}", "<module>", "<main>":
		return "" // no name to group or search by
	}
	if m := reAsyncName.FindStringSubmatch(fn); m != nil {
		return m[1]
	}
	if i := strings.IndexByte(fn, '('); i >= 0 {
		fn = fn[:i]
	}
	if i := strings.Index(fn, " [as "); i >= 0 { // V8: "Layer.handle [as handle_request]"
		fn = fn[:i]
	}
	fn = reLambda.ReplaceAllString(fn, "")
	fn = strings.Trim(fn, ". ")
	// Return types and modifiers ("Task<IActionResult> Create") come first.
	if i := strings.LastIndexByte(fn, ' '); i >= 0 {
		fn = fn[i+1:]
	}
	return fn
}

// AppFrame is the app's own frame nearest to the crash in the root cause
// (falling back to the outer exceptions), or a zero frame.
func (e ServerError) AppFrame() ServerFrame {
	for i := 0; i < len(e.Chain); i++ {
		fs := e.Chain[i].Frames
		for j := len(fs) - 1; j >= 0; j-- {
			if frameInApp(fs[j]) && CleanFunction(fs[j].Function) != "" {
				return fs[j]
			}
		}
	}
	return ServerFrame{}
}

// AppFunction is "Module.Function" of AppFrame, without line numbers.
func (e ServerError) AppFunction() string {
	f := e.AppFrame()
	fn := CleanFunction(f.Function)
	if fn == "" {
		return ""
	}
	mod := strings.TrimSpace(f.Module)
	if mod == "" || strings.HasSuffix(fn, mod) || strings.HasPrefix(fn, mod+".") {
		return fn
	}
	// "FoodsController.Create" in module "App.Controllers.FoodsController".
	if i := strings.LastIndexByte(fn, '.'); i > 0 && (mod == fn[:i] || strings.HasSuffix(mod, "."+fn[:i])) {
		return mod + fn[i:]
	}
	return mod + "." + fn
}

var reMethodPath = regexp.MustCompile(`^(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(\S+)$`)

// Route is the templated request path, from the URL, else from a
// transaction named like "POST /orders/{id}".
func (e ServerError) Route() string {
	if e.URL != "" {
		return RouteTemplate(e.URL)
	}
	if m := reMethodPath.FindStringSubmatch(strings.TrimSpace(e.Transaction)); m != nil {
		p := m[1]
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return RouteTemplate(p)
	}
	return ""
}

// where identifies the request or job: the route, else the transaction name
// (background jobs have no route).
func (e ServerError) where() string {
	if r := e.Route(); r != "" {
		return r
	}
	return e.Transaction
}

// ServerFingerprint hashes chain types + root message + app function + route.
func ServerFingerprint(e ServerError) string {
	return "v2:" + hashHex("v2|server|"+e.Types()+"|"+MessageV2(e.Root().Message)+"|"+e.AppFunction()+"|"+e.where())
}

// ServerSignature is the readable form used for titles and the knowledge base.
func ServerSignature(e ServerError, t Tags) string {
	root := e.Root()
	s := "server " + e.Platform + " " + ShortType(root.Type) + " \"" + MessageV2(root.Message) + "\""
	if len(e.Chain) > 1 {
		s += " in " + ShortType(e.Outer().Type)
	}
	if w := e.where(); w != "" {
		s += " route=" + w
	}
	if fn := e.AppFunction(); fn != "" {
		s += " fn=" + fn
	}
	return s + t.suffix()
}

// ServerFrameInApp reports whether a frame is the app's own code: the SDK's
// in_app flag, else not a known library or framework path.
func ServerFrameInApp(f ServerFrame) bool { return frameInApp(f) }
