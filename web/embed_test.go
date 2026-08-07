// embed_test.go — the serving contract the operator (and `jig serve`)
// depends on: app routes fall back to the SPA, missing assets stay 404s,
// hashed assets are immutable, and the bundle is actually embedded.
package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesTheEmbeddedIndex(t *testing.T) {
	handler := Handler()
	for _, path := range []string{"/", "/runs", "/runs/run-123", "/jobs/job-1", "/worktrees"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), `<div id="root">`) {
			t.Fatalf("GET %s: expected the SPA shell", path)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("GET %s: the shell must never be cached, got %q", path, got)
		}
		if recorder.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("GET %s: browser hardening headers are missing", path)
		}
	}
}

func TestMissingAssetsStay404(t *testing.T) {
	handler := Handler()
	for _, path := range []string{"/assets/missing.js", "/favicon.ico", "/styles/app.css"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("GET %s: a missing asset must 404, got %d", path, recorder.Code)
		}
	}
}

func TestHashedAssetsAreImmutable(t *testing.T) {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(dist, "assets")
	if err != nil {
		t.Fatalf("the committed bundle has no assets directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the committed bundle is empty; run npm run build in web/")
	}
	handler := Handler()
	for _, entry := range entries {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/"+entry.Name(), nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /assets/%s: expected 200, got %d", entry.Name(), recorder.Code)
		}
		if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
			t.Fatalf("GET /assets/%s: expected immutable caching, got %q", entry.Name(), got)
		}
	}
}

func TestNonReadMethodsAreRefused(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d", recorder.Code)
	}
}
