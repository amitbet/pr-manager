package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/amitbet/pr-manager/triage"
)

// decisionDir stores classifier decisions as one JSON file per key, so a
// unit that did not change between runs is not classified again.
type decisionDir string

func (d decisionDir) path(key string) string {
	return filepath.Join(string(d), key[:2], key+".json")
}

func (d decisionDir) Load(key string) (triage.Decision, bool) {
	var dec triage.Decision
	b, err := os.ReadFile(d.path(key))
	if err != nil || json.Unmarshal(b, &dec) != nil || !dec.Bucket.Valid() {
		return triage.Decision{}, false
	}
	return dec, true
}

// Save writes through a temporary file, so a concurrent Load never reads
// half a decision. Errors only cost a later run a call.
func (d decisionDir) Save(key string, dec triage.Decision) {
	b, err := json.Marshal(dec)
	if err != nil {
		return
	}
	p := d.path(key)
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), p) != nil {
		os.Remove(tmp.Name())
	}
}
