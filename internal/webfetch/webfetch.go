// Package webfetch GETs a public web page as text, for the chat agent:
// the app's MCP server gives it to codex, and the API providers' tool loop
// to theirs (Claude Code has its own). Addresses on this machine and the
// private network are refused, so the agent can't reach the app or the
// LAN.
package webfetch

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Max is the most chars of a page returned.
const Max = 200 << 10

// Description is the tool's description, the same for every provider.
const Description = "GET a public http or https URL and return its text (HTML is reduced to text), such as a library's docs, a changelog, or an issue on another site."

// Fetch GETs u and returns its status, final URL and text.
func Fetch(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("not an http(s) URL: %q", raw)
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("%s is not a public address", host)
		}
		return nil
	}}
	client := &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, Proxy: nil},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "pr-manager")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	text := string(b)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		text = Text(text)
	}
	out := fmt.Sprintf("%s %s\n\n%s", resp.Status, resp.Request.URL, clip(text, Max))
	if resp.StatusCode >= 400 {
		return "", errors.New(clip(out, 4000))
	}
	return out, nil
}

var (
	drop       = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)\b.*?</(script|style|noscript|svg|head)>`)
	breaks     = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr|/pre)\s*/?>`)
	tags       = regexp.MustCompile(`(?s)<[^>]*>`)
	blankLines = regexp.MustCompile(`\n\s*\n+`)
	spaces     = regexp.MustCompile(`[ \t]+`)
)

// Text reduces HTML to its text.
func Text(s string) string {
	s = drop.ReplaceAllString(s, "")
	s = breaks.ReplaceAllString(s, "\n")
	s = tags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(blankLines.ReplaceAllString(s, "\n\n"))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[cut]\n"
}
