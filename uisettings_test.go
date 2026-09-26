package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUISettings(t *testing.T) {
	mux := http.NewServeMux()
	newUISettings(t.TempDir()).routes(mux)
	post := func(body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/settings", strings.NewReader(body)))
		return rec.Code
	}
	script := func() string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/settings.js", nil))
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}

	if got := script(); got != "window.SAVED_SETTINGS = {};\n" {
		t.Errorf("empty: %q", got)
	}
	if c := post(`{"pr-manager.summary_lang":"Hebrew","pr-manager.view":"split","pr-manager.x":"</script>"}`); c != 204 {
		t.Fatalf("post = %d", c)
	}
	if c := post(`{"pr-manager.view":null}`); c != 204 {
		t.Fatalf("delete = %d", c)
	}
	got := script()
	if !strings.Contains(got, `"pr-manager.summary_lang":"Hebrew"`) || strings.Contains(got, "pr-manager.view") {
		t.Errorf("after set and delete: %q", got)
	}
	if strings.Contains(got, "</script>") {
		t.Errorf("a value can end the script: %q", got)
	}
	if c := post(`{"other":"x"}`); c != 400 {
		t.Errorf("a key without the prefix: %d, want 400", c)
	}
}
