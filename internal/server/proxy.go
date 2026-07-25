package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
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

// proxyTo forwards the (already-read) request body to baseURL+path and streams
// the response back, flushing each chunk so SSE tokens arrive live. The client's
// Authorization is intentionally not forwarded — backend engines are local and
// unauthenticated; Mainspring is the trust boundary.
//
// Transient failures (connection errors, retryable statuses) are retried with
// exponential backoff up to s.retryMax additional attempts. Retry is always
// decided *before* any byte reaches the client, so it is safe for both
// streaming and non-streaming responses. It returns the number of retries the
// request incurred (0 when it succeeded first try).
func (s *Server) proxyTo(w http.ResponseWriter, r *http.Request, baseURL string, body []byte, extra map[string]string) int {
	attempts := s.retryMax + 1
	for attempt := 0; ; attempt++ {
		if attempt > 0 && !sleepBackoff(r.Context(), s.retryBackoff, attempt) {
			writeErr(w, codeTimeout, "request timed out")
			return attempt
		}
		last := attempt == attempts-1

		resp, err := s.upstreamDo(r, baseURL, body)
		if err != nil {
			if r.Context().Err() == context.DeadlineExceeded {
				writeErr(w, codeTimeout, "request timed out")
				return attempt
			}
			if !last {
				continue // transient connection error → retry
			}
			writeError(w, http.StatusBadGateway, "backend request failed: "+err.Error())
			return attempt
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
		s.commitResponse(w, resp, extra)
		_ = resp.Body.Close()
		return attempt
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

// commitResponse copies the upstream headers (minus hop-by-hop), applies the
// fail-loud extras, writes the status, and streams the body to the client.
func (s *Server) commitResponse(w http.ResponseWriter, resp *http.Response, extra map[string]string) {
	for k, vs := range resp.Header {
		if hopByHop[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
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

// flushCopy streams src to w, flushing after every chunk for live SSE delivery.
func flushCopy(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}
