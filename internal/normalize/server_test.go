package normalize

import "testing"

func TestCleanFunction(t *testing.T) {
	cases := map[string]string{
		"Create(Product product)": "Create",
		"Task<IActionResult> ProductsController.Create(Product product)": "ProductsController.Create",
		"<Create>d__5.MoveNext()":          "Create",
		"MoveNext() in <Create>d__12":      "Create",
		"?":                                "",
		"<anonymous>":                      "",
		"<module>":                         "",
		"import_signup":                    "import_signup",
		"Layer.handle [as handle_request]": "Layer.handle",
		"App\\Http\\Controllers\\OrderController::store": "App\\Http\\Controllers\\OrderController::store",
	}
	for in, want := range cases {
		if got := CleanFunction(in); got != want {
			t.Errorf("CleanFunction(%q) = %q, want %q", in, got, want)
		}
	}
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// dotnet is the shape of an Entity Framework save failure: the root cause is
// thrown inside the framework; the app's frame is in the wrapper.
func dotnet(value string, line int, fn string) ServerError {
	return ServerError{
		Platform: "csharp", URL: "https://shop.example.com/Products/Create?id=9",
		Chain: []ServerException{
			{Type: "System.ArgumentException", Message: "Parameter value '" + value + "' is out of range.",
				Frames: []ServerFrame{{Module: "Microsoft.EntityFrameworkCore.Storage.RelationalCommand", Function: "ExecuteReaderAsync()", InApp: no()}}},
			{Type: "Microsoft.EntityFrameworkCore.DbUpdateException", Message: "An error occurred while saving the entity changes.",
				Frames: []ServerFrame{
					{Module: "Shop.Controllers.ProductsController", Function: fn, Filename: "Controllers/ProductsController.cs", Lineno: line, InApp: yes()},
					{Module: "Microsoft.EntityFrameworkCore.DbContext", Function: "SaveChangesAsync()", InApp: no()},
				}},
		},
	}
}

func TestServerFingerprint(t *testing.T) {
	a := dotnet("7771.00", 58, "Create(Product product)")
	if got := a.AppFunction(); got != "Shop.Controllers.ProductsController.Create" {
		t.Errorf("AppFunction = %q", got)
	}
	if got := a.Route(); got != "/Products/Create" {
		t.Errorf("Route = %q", got)
	}
	if got := a.Types(); got != "ArgumentException<DbUpdateException" {
		t.Errorf("Types = %q", got)
	}
	// Another value, an edited file (new line number): the same issue.
	if ServerFingerprint(a) != ServerFingerprint(dotnet("9000.50", 61, "Create(Product product)")) {
		t.Error("value or line number split the issue")
	}
	// Another action failing the same way: a different issue.
	if ServerFingerprint(a) == ServerFingerprint(dotnet("7771.00", 58, "Edit(int id, Product product)")) {
		t.Error("different functions share a fingerprint")
	}
	// Browser and server errors never share a fingerprint scheme.
	if ServerFingerprint(a)[:3] != "v2:" {
		t.Error("unexpected prefix")
	}
	sig := ServerSignature(a, Tags{Platform: "cursor", Stack: "unknown"})
	want := `server csharp ArgumentException "Parameter value <str> is out of range." in DbUpdateException route=/Products/Create fn=Shop.Controllers.ProductsController.Create platform=cursor stack=unknown`
	if sig != want {
		t.Errorf("signature\n got %s\nwant %s", sig, want)
	}
}

func TestServerRouteAndLibraryFrames(t *testing.T) {
	// A background job: no URL; the transaction names it.
	job := ServerError{Platform: "python", Transaction: "tasks.send_invoices",
		Chain: []ServerException{{Type: "KeyError", Message: "'email'", Frames: []ServerFrame{{Module: "tasks", Function: "send_invoices", Filename: "tasks.py", InApp: yes()}}}}}
	if job.Route() != "" || job.where() != "tasks.send_invoices" {
		t.Errorf("job route=%q where=%q", job.Route(), job.where())
	}
	// "POST /orders/{id}" transactions give a route when there's no URL.
	tx := ServerError{Transaction: "POST /orders/123"}
	if r := tx.Route(); r != "/orders/<id>" && r != "/orders/123" {
		t.Errorf("transaction route = %q", r)
	}
	// Library frames are never the app's, even when the SDK says in_app.
	lib := ServerError{Chain: []ServerException{{Type: "TypeError", Frames: []ServerFrame{
		{Function: "handle", Filename: `C:\app\node_modules\express\lib\router\layer.js`, InApp: yes()},
		{Function: "?", Filename: "/app/server.js", InApp: yes()},
	}}}}
	if fn := lib.AppFunction(); fn != "" {
		t.Errorf("AppFunction = %q, want none (anonymous app frame, library frame)", fn)
	}
}
