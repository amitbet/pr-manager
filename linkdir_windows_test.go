package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/amitbet/pr-manager/codemap"
)

// A junction is what a checkout is linked in with when symlinks need a
// privilege: it must read as a link to the checkout, resolve to it, and
// come off without taking the checkout with it.
func TestJunction(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "api")
	if err := os.MkdirAll(filepath.Join(src, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "pkg", "a.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "code", "api")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := createJunction(src, link); err != nil {
		t.Fatal(err)
	}
	if !isLink(link) || !linksTo(link, src) {
		t.Error("the junction does not read as a link to the checkout")
	}
	if _, err := os.ReadFile(filepath.Join(link, "pkg", "a.go")); err != nil {
		t.Errorf("reading through the junction: %v", err)
	}
	want, _ := filepath.EvalSymlinks(src)
	if got, err := codemap.ResolveLink(link); err != nil || got != want {
		t.Errorf("ResolveLink = %q %v, want %q", got, err, want)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "pkg", "a.go")); err != nil {
		t.Errorf("removing the junction removed the checkout: %v", err)
	}
}
