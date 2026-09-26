package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// uiSettings keeps the UI's pr-manager.* localStorage keys in a file. The
// browser keeps localStorage per origin, so without it a server on another
// port, or another browser, starts from defaults. ui/js/persist.js restores
// the file into localStorage before the UI loads and sends changes back.
type uiSettings struct {
	path string
	mu   sync.Mutex
}

const (
	uiSettingsPrefix = "pr-manager."
	uiSettingsMax    = 1 << 20 // bytes of request body
)

func (s *uiSettings) load() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// update sets each key to its value; a null value deletes the key.
func (s *uiSettings) update(changes map[string]*string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.load()
	for k, v := range changes {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = *v
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *uiSettings) routes(mux *http.ServeMux) {
	// A script, not JSON, so it can run before the UI's modules read
	// localStorage. json.Marshal escapes <, > and &, so the values can't
	// end the script.
	mux.HandleFunc("GET /api/settings.js", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		b, _ := json.Marshal(s.load())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "window.SAVED_SETTINGS = %s;\n", b)
	})
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var changes map[string]*string
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, uiSettingsMax)).Decode(&changes); err != nil {
			writeErr(w, 400, err)
			return
		}
		for k := range changes {
			if !strings.HasPrefix(k, uiSettingsPrefix) {
				writeErr(w, 400, fmt.Errorf("setting %q is not %s*", k, uiSettingsPrefix))
				return
			}
		}
		if err := s.update(changes); err != nil {
			writeErr(w, 500, err)
			return
		}
		w.WriteHeader(204)
	})
}

func newUISettings(cache string) *uiSettings {
	return &uiSettings{path: filepath.Join(cache, "ui-settings.json")}
}
