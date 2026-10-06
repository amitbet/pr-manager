package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/llm"
)

// A snapshot is the page as the reader sees it, without the chat (the
// snapshot action, ui/js/snapshot.js): the UI sends it, it is kept with
// the conversation under <cache>/chats/<hash>/snapshots, and the action's
// event names its file as [snapshot: <path>]. The agent's next turn gets
// the snapshots its new turns name: codex as attached images, Claude Code
// by reading the file. The API providers get the page's text, which the
// event carries too.

const (
	chatSnapshotMax  = 16 << 20 // bytes of a snapshot request
	chatSnapshotKeep = 20       // snapshots a conversation keeps
)

var snapshotRef = regexp.MustCompile(`\[snapshot: ([^\]\n]+\.png)\]`)

func (t *triager) snapshotDir(change string) string {
	return filepath.Join(t.chatsDir(), convHash(change), "snapshots")
}

// saveSnapshot keeps a PNG data URL for change's conversation.
func (t *triager) saveSnapshot(change, dataURL string) (string, error) {
	b64, ok := strings.CutPrefix(dataURL, "data:image/png;base64,")
	if !ok {
		return "", errors.New("not a PNG data URL")
	}
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	if len(b) < 8 || string(b[1:4]) != "PNG" {
		return "", errors.New("not a PNG")
	}
	dir := t.snapshotDir(change)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, fmt.Sprintf("%d.png", time.Now().UnixMilli()))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return "", err
	}
	if es, err := os.ReadDir(dir); err == nil && len(es) > chatSnapshotKeep {
		for _, e := range es[:len(es)-chatSnapshotKeep] { // names sort by time
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return p, nil
}

// snapshots gives ws the conversation's snapshot folder to read, and the
// snapshots the turns since the agent last answered name, for this turn.
func (cv *chatConv) snapshots(ws *llm.Workspace) {
	if cv.id == "" || ws == nil {
		return
	}
	dir := cv.t.snapshotDir(cv.id)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	ws.ReadDirs = append(ws.ReadDirs, dir)
	for i := len(cv.turns) - 1; i >= 0 && cv.turns[i].Role != "assistant"; i-- {
		for _, m := range snapshotRef.FindAllStringSubmatch(cv.turns[i].Content, -1) {
			p := filepath.Clean(m[1])
			if filepath.Dir(p) != dir {
				continue // only the conversation's own
			}
			if _, err := os.Stat(p); err == nil {
				ws.Images = append(ws.Images, p)
			}
		}
	}
}
