package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ankit373/mainspring/internal/apierr"
)

// proxyClient streams from backend runners. No overall timeout — generations and
// SSE streams run long; cancellation flows through the request context instead.
var proxyClient = &http.Client{
	Timeout: 0,
	Transport: &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	},
}

// maxRetryBackoff caps the exponential backoff between retries.
const maxRetryBackoff = 5 * time.Second

// hopByHop headers must not be forwarded across a proxy hop.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
}

// SetRetry configures bounded retry of transient upstream failures. maxRetries
// is the number of *additional* attempts after the first (0 disables retry);
// backoff is the base of the exponential delay. Safe to call at startup.
func (s *Server) SetRetry(maxRetries int, backoff time.Duration) {
	s.retryMax = maxRetries
	s.retryBackoff = backoff
}

// retryableStatus reports whether an upstream status is worth retrying: gateway
// and unavailability classes that are typically transient (a briefly-busy or
// restarting engine). A 500 is left alone — it may be a deterministic error.
func retryableStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// proxyResult reports what a proxied request actually did, beyond what the
// status line can express. Once a response is committed the status is on the
// wire — pinned at 200 for a stream — so a body that stops early needs an
// explicit signal or it is indistinguishable from a complete answer.
type proxyResult struct {
	retries       int  // additional upstream attempts incurred (0 = succeeded first try)
	incomplete    bool // the body did not arrive in full → never cacheable or shareable
	backendFailed bool // the break was the backend's fault (not a caller's own abort)
	abandoned     bool // the caller went away; the request says nothing about the backend
}

// clientGone reports whether the caller cancelled its own request. Such a
// request gets no response written — the connection is already gone — and is
// never charged to the circuit breaker, since otherwise enough client
// disconnects would open the circuit for every tenant. A deadline is
// deliberately excluded: a timeout *is* a backend failure.
func clientGone(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// proxyTo forwards the (already-read) request body to baseURL+path and streams
// the response back, flushing each chunk so SSE tokens arrive live. The client's
// Authorization is intentionally not forwarded — backend engines are local and
// unauthenticated; Mainspring is the trust boundary.
//
// Transient failures (connection errors, retryable statuses) are retried with
// exponential backoff up to s.retryMax additional attempts. Retry is always
// decided *before* any byte reaches the client, so it is safe for both
// streaming and non-streaming responses.
//
// cc is non-nil only when the caller asked for n>1 candidates, and is what makes
// an engine that quietly returned fewer say so. Nil — the overwhelmingly common
// case — leaves this a pure byte-shoveller.
func (s *Server) proxyTo(w http.ResponseWriter, r *http.Request, baseURL string, body []byte, extra map[string]string, cc *choiceCheck) proxyResult {
	// Apply the fail-loud extras (backend, device, degraded warning, served model)
	// to the header map once, up front. Applying them only on the commit path is
	// what left every error return below — timeout, 502, retries exhausted —
	// silent about which backend and which model the failure came from.
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	attempts := s.retryMax + 1
	for attempt := 0; ; attempt++ {
		if attempt > 0 && !sleepBackoff(r.Context(), s.retryBackoff, attempt) {
			if clientGone(r.Context()) {
				return proxyResult{retries: attempt, abandoned: true}
			}
			writeErr(w, codeTimeout, "request timed out")
			return proxyResult{retries: attempt}
		}
		last := attempt == attempts-1

		resp, err := s.upstreamDo(r, baseURL, body)
		if err != nil {
			if clientGone(r.Context()) {
				return proxyResult{retries: attempt, abandoned: true}
			}
			if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
				writeErr(w, codeTimeout, "request timed out")
				return proxyResult{retries: attempt}
			}
			if !last {
				continue // transient connection error → retry
			}
			writeError(w, http.StatusBadGateway, "backend request failed: "+err.Error())
			return proxyResult{retries: attempt}
		}

		if !last && retryableStatus(resp.StatusCode) {
			// Drain a bounded amount so the connection can be reused, then retry.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			continue
		}

		// Commit: nothing has been written to the client yet.
		if attempt > 0 {
			w.Header().Set("X-Mainspring-Retries", strconv.Itoa(attempt))
		}
		copyErr := s.commitResponse(w, resp, cc)
		stream := isEventStream(resp.Header.Get("Content-Type"))
		_ = resp.Body.Close()
		if copyErr == nil {
			return proxyResult{retries: attempt}
		}
		// The answer was severed. The status is already on the wire, so for a
		// stream a terminal error frame is the only signal the client will get.
		clientLeft := clientGone(r.Context()) || errors.Is(copyErr, errClientWrite)
		if stream {
			switch {
			case errors.Is(r.Context().Err(), context.DeadlineExceeded):
				sseError(w, codeTimeout, "stream exceeded the per-request timeout")
			case clientLeft:
				sseError(w, codeInternal, "stream cancelled before completion")
			default:
				sseError(w, codeUpstreamError, "upstream stream ended prematurely: "+copyErr.Error())
			}
		}
		return proxyResult{retries: attempt, incomplete: true, backendFailed: !clientLeft, abandoned: clientLeft}
	}
}

// sseError emits a terminal `error` frame on an already-open SSE stream, in the
// OpenAI dialect and carrying Mainspring's error taxonomy — the counterpart to
// streamError on the Anthropic path.
func sseError(w http.ResponseWriter, code errorCode, msg string) {
	b, _ := json.Marshal(apierr.Body(code, msg))
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

// upstreamDo builds and issues one upstream request with a fresh body reader.
func (s *Server) upstreamDo(r *http.Request, baseURL string, body []byte) (*http.Response, error) {
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, baseURL+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	outReq.Header.Set("Content-Type", "application/json")
	if a := r.Header.Get("Accept"); a != "" {
		outReq.Header.Set("Accept", a)
	}
	// Forward the child trace context so the upstream engine joins the trace.
	if tp := outgoingTraceparent(r.Context()); tp != "" {
		outReq.Header.Set("traceparent", tp)
	}
	return proxyClient.Do(outReq)
}

// commitResponse copies the upstream headers (minus hop-by-hop), writes the
// status, and streams the body to the client. It returns the body-copy error,
// nil only when the whole body was delivered. The fail-loud extras are already
// on the header map (see proxyTo) and win: a header we set describes this hop,
// so an upstream that echoes the same name — Mainspring proxying Mainspring —
// must not append a second, stale value.
//
// cc (nil unless the caller asked for n>1) is the one thing that reads the body
// on the way past: a non-streaming answer is buffered before the status line so a
// shortfall can still be reported in a header, and a stream is counted as it
// flows. Its warning composes onto whatever the header already carries, so a
// degraded device and a short `n` are both reported.
func (s *Server) commitResponse(w http.ResponseWriter, resp *http.Response, cc *choiceCheck) error {
	src, warn, preErr := cc.observe(resp)
	for k, vs := range resp.Header {
		ck := http.CanonicalHeaderKey(k)
		if hopByHop[ck] || w.Header().Get(ck) != "" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(ck, v)
		}
	}
	applyShortfall(w.Header(), warn)
	w.WriteHeader(resp.StatusCode)
	if preErr != nil {
		// The prefix we did read still belongs to the client; the read error is what
		// marks the answer incomplete.
		_ = flushCopy(w, src)
		return preErr
	}
	if err := flushCopy(w, src); err != nil {
		return err
	}
	cc.emitShortfall(w)
	return nil
}

// sleepBackoff waits an exponential delay before retry `attempt` (1-based),
// capped at maxRetryBackoff and cancellable via ctx. Returns false if ctx ended
// during the wait.
func sleepBackoff(ctx context.Context, base time.Duration, attempt int) bool {
	if base <= 0 {
		return ctx.Err() == nil
	}
	d := base << (attempt - 1)
	if d > maxRetryBackoff || d <= 0 { // <=0 guards shift overflow
		d = maxRetryBackoff
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// errClientWrite marks a copy that failed writing to the *client* rather than
// reading from the upstream: the caller's connection died, which is never the
// backend's fault. The request context alone cannot say so — it races the write
// and may not be cancelled yet.
var errClientWrite = errors.New("write to client failed")

// flushCopy streams src to w, flushing after every chunk for live SSE delivery.
// It returns nil only when src ended at io.EOF and every byte reached the
// client; any other terminal condition is returned, because a caller that
// cannot tell a finished body from a severed one will serve — and cache — a
// truncation as a success.
func flushCopy(w http.ResponseWriter, src io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return fmt.Errorf("%w: %v", errClientWrite, werr)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}
