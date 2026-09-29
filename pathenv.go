package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/amitbet/pr-manager/internal/proc"
)

// inheritShellPath widens PATH for a GUI launch. Apps started from Finder or
// the Dock get launchd's minimal PATH (/usr/bin:/bin:/usr/sbin:/sbin), so gh,
// git, claude and codex installed via Homebrew or npm are not found. Ask the
// user's login shell for its PATH and fall back to the usual install dirs.
func inheritShellPath() {
	if runtime.GOOS == "windows" {
		return
	}
	home, _ := os.UserHomeDir()
	fallback := []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin"}
	if home != "" {
		fallback = append(fallback, filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"))
	}
	// The login shell's order wins, so Homebrew's git beats /usr/bin/git as
	// it does in a terminal; launchd's dirs and the fallbacks fill gaps.
	os.Setenv("PATH", mergePath(loginShellPath(), os.Getenv("PATH"), strings.Join(fallback, string(os.PathListSeparator))))
}

const pathMarker = "__PR_MANAGER_PATH__"

// loginShellTimeout bounds the PATH probe, which delays startup. Interactive
// rc files with oh-my-zsh, nvm or conda can take several seconds.
const loginShellTimeout = 10 * time.Second

// loginShellPath returns the PATH an interactive login shell sets up, or ""
// if the shell fails or takes too long.
func loginShellPath() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginShellTimeout)
	defer cancel()
	// Interactive so .zshrc/.bashrc run too; the markers skip anything the rc
	// files print.
	cmd := proc.CommandContext(ctx, shell, "-ilc", `printf '%s%s%s' "`+pathMarker+`" "$PATH" "`+pathMarker+`"`)
	// Something the rc files background can inherit stdout and keep it open
	// after the shell exits; don't wait on it past WaitDelay.
	cmd.WaitDelay = time.Second
	reap := ownGroup(cmd)
	out, _ := cmd.Output()
	reap()
	// The markers show the PATH was printed in full, even if Output then
	// failed on a held pipe or the shell's exit status.
	parts := strings.Split(string(out), pathMarker)
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-2]
}

// mergePath joins PATH lists in order, dropping empty and repeated entries.
func mergePath(lists ...string) string {
	seen := map[string]bool{}
	var dirs []string
	for _, l := range lists {
		for _, d := range filepath.SplitList(l) {
			if d != "" && !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}
