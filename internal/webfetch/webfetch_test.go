package webfetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Fetch won't reach this machine or the LAN, nor anything but http(s).
func TestFetchRefusesLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	if out, err := Fetch(context.Background(), srv.URL); err == nil {
		t.Errorf("fetched a local address: %q", out)
	}
	if _, err := Fetch(context.Background(), "file:///etc/passwd"); err == nil {
		t.Error("fetched a file URL")
	}
	if got := Text("<html><head><title>x</title></head><body><script>bad()</script><p>Hello &amp; welcome</p><div>bye</div></body></html>"); got != "Hello & welcome\nbye" {
		t.Errorf("Text = %q", got)
	}
}
