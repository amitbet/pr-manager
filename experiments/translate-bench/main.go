// translate-bench times triage.Translate on a cached result with several
// models and settings, and writes each output for comparison.
//
//	go run ./experiments/translate-bench -file result.json -runs 'claude-code/claude-haiku-4-5@;codex/gpt-5.6-luna@minimal'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/llm"
	"github.com/amitbet/pr-manager/triage"
)

func main() {
	file := flag.String("file", "", "cached result JSON")
	runs := flag.String("runs", "", "provider/model@effort;...")
	lang := flag.String("lang", "Hebrew", "language")
	chars := flag.Int("chars", triage.TranslateBatchChars, "batch chars")
	conc := flag.Int("conc", triage.TranslateConcurrency, "concurrency")
	out := flag.String("out", "/tmp/tbench", "output dir")
	flag.Parse()
	b, err := os.ReadFile(*file)
	if err != nil {
		panic(err)
	}
	var r struct {
		Files []struct {
			Units []*triage.Unit `json:"units"`
		} `json:"files"`
		Overview *triage.Overview `json:"overview"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		panic(err)
	}
	texts := map[string]triage.UnitText{}
	for _, f := range r.Files {
		for _, u := range f.Units {
			texts[u.ID] = triage.TextOf(u)
		}
	}
	j, _ := json.Marshal(texts)
	fmt.Printf("units=%d json=%d chars\n", len(texts), len(j))
	triage.TranslateBatchChars, triage.TranslateConcurrency = *chars, *conc
	os.MkdirAll(*out, 0o755)
	for _, s := range strings.Split(*runs, ";") {
		pm, effort, _ := strings.Cut(s, "@")
		prov, model, _ := strings.Cut(pm, "/")
		l, err := llm.New(prov, model)
		if err != nil {
			panic(err)
		}
		llm.SetEffort(l, effort)
		start := time.Now()
		units, ov, err := triage.Translate(context.Background(), l, *lang, texts, r.Overview)
		d := time.Since(start)
		name := fmt.Sprintf("%s_%s_%s_c%d_b%d", prov, model, orStr(effort, "default"), *conc, *chars)
		if err != nil {
			fmt.Printf("RESULT %-58s FAIL %6.1fs %v\n", name, d.Seconds(), err)
			continue
		}
		o, _ := json.MarshalIndent(map[string]any{"units": units, "overview": ov}, "", " ")
		os.WriteFile(filepath.Join(*out, name+".json"), o, 0o644)
		fmt.Printf("RESULT %-58s OK   %6.1fs\n", name, d.Seconds())
	}
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
