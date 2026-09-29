package llm

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// retryPolicy is how an HTTP provider rides out a transient failure: a 429
// or overloaded burst from running reviews concurrently shouldn't fail the
// unit outright.
type retryPolicy struct {
	MaxRetries int           // attempts after the first
	Base, Max  time.Duration // exponential backoff: Base, 2*Base, ... capped at Max
	MaxWait    time.Duration // cap on a server-advised wait (Retry-After and friends)
	Sleep      func(ctx context.Context, d time.Duration) error
}

var httpRetry = retryPolicy{
	MaxRetries: 4,
	Base:       time.Second,
	Max:        30 * time.Second,
	MaxWait:    60 * time.Second,
	Sleep:      sleepCtx,
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryableStatus: rate limited, request timeout, and the 5xx a retry can
// fix (529 is Anthropic's "overloaded"). Other 4xx are the request's fault.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

// transientErr: the connection dropped, or timed out while connecting, not
// because the caller's context ended. The client's whole-request timeout
// isn't retried: that call already waited minutes, and the next would too.
func transientErr(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial" && oe.Timeout()
}

// serverWait is the wait the response asks for, if any.
func serverWait(h http.Header, status int, now time.Time) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("retry-after-ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
			return time.Duration(s * float64(time.Second)), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return max(t.Sub(now), 0), true
		}
	}
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	// OpenAI: durations like "1s", "6m0s", "20ms". Wait for the later one.
	var wait time.Duration
	var ok bool
	for _, k := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "x-ratelimit-reset"} {
		v := strings.TrimSpace(h.Get(k))
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			s, serr := strconv.ParseFloat(v, 64)
			if serr != nil || s < 0 {
				continue
			}
			d = time.Duration(s * float64(time.Second))
		}
		wait, ok = max(wait, d), true
	}
	return wait, ok
}

// backoff is the jittered wait before retry n (0-based): about Base*2^n,
// between half and one and a half of it, capped at Max.
func (p retryPolicy) backoff(n int) time.Duration {
	d := p.Base << min(n, 16)
	if d <= 0 || d > p.Max {
		d = p.Max
	}
	d = d/2 + time.Duration(rand.Int64N(int64(d)+1))
	return min(d, p.Max)
}

// doRetry sends the request newReq builds and reads the whole body,
// retrying transient failures. newReq runs for every attempt, so the body is
// fresh and auth (including a SigV4 timestamp) is redone; its errors are not
// retried. The final attempt's status and body are returned whatever the
// status, so callers keep their own error handling.
func doRetry(ctx context.Context, client *http.Client, newReq func() (*http.Request, error)) (int, []byte, error) {
	return httpRetry.do(ctx, client, newReq)
}

func (p retryPolicy) do(ctx context.Context, client *http.Client, newReq func() (*http.Request, error)) (int, []byte, error) {
	for n := 0; ; n++ {
		req, err := newReq()
		if err != nil {
			return 0, nil, err
		}
		status, header, body, err := sendOnce(client, req)
		last := n >= p.MaxRetries || ctx.Err() != nil
		var wait time.Duration
		switch {
		case err != nil:
			if last || !transientErr(ctx, err) {
				return 0, nil, err
			}
			wait = p.backoff(n)
		case retryableStatus(status) && !last && header.Get("x-should-retry") != "false":
			if d, ok := serverWait(header, status, time.Now()); ok {
				wait = min(d, p.MaxWait)
			} else {
				wait = p.backoff(n)
			}
		default:
			return status, body, nil
		}
		if err := p.Sleep(ctx, wait); err != nil {
			return 0, nil, err
		}
	}
}

func sendOnce(client *http.Client, req *http.Request) (int, http.Header, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}
