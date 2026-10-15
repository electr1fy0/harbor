package web

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestTemplEscapesPageAndFormData(t *testing.T) {
	payload := `<script>alert("secret")</script>`
	page := Page{Title: payload, Path: "/", CSS: "/assets/app.css", JS: "/assets/app.js", Sections: []Section{{ID: "test", Title: payload, Log: payload, Table: Table{Headers: []string{"Example"}, Rows: []Row{{Cells: []Cell{{Text: payload}}}}}}}, Forms: []Form{{ID: "test-form", Function: "configureApp", Submit: "Save", Fields: []Field{{Name: "environment", Label: payload, Value: payload, Type: "textarea"}}}}}
	var output bytes.Buffer
	if err := Dashboard(page).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	if strings.Contains(html, payload) || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("unescaped component data: %s", html)
	}
	if strings.Contains(html, `<script>`) || strings.Contains(html, `onclick=`) {
		t.Fatal("component added an inline executable script")
	}
}

func TestGeneratedPagesUseOnlyEmbeddedBrowserAssets(t *testing.T) {
	for _, path := range []string{"/", "/login", "/new", "/projects/demo"} {
		var output bytes.Buffer
		if err := Dashboard(Page{Path: path, CSS: "/assets/app.css?v=123", JS: "/assets/app.js?v=456", Public: true}).Render(context.Background(), &output); err != nil {
			t.Fatal(err)
		}
		html := output.String()
		for _, asset := range []string{`href="/assets/app.css?v=123"`, `src="/assets/app.js?v=456"`} {
			if !strings.Contains(html, asset) {
				t.Fatalf("%s: missing %s", path, asset)
			}
		}
		if strings.Contains(html, "https://") || strings.Contains(html, `id="root"`) {
			t.Fatalf("%s requires an external asset or SPA bootstrap", path)
		}
	}
}
