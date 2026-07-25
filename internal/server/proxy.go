package server

import (
	"bytes"
	"io"
	"net/http"
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

// proxyTo forwards the (already-read) request body to baseURL+path and streams
// the response back, flushing each chunk so SSE tokens arrive live. The client's
// Authorization is intentionally not forwarded — backend engines are local and
// unauthenticated; Mainspring is the trust boundary.
func (s *Server) proxyTo(w http.ResponseWriter, r *http.Request, baseURL string, body []byte, extra map[string]string) {
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, baseURL+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "build upstream request: "+err.Error())
		return
	}
	outReq.Header.Set("Content-Type", "application/json")
	if a := r.Header.Get("Accept"); a != "" {
		outReq.Header.Set("Accept", a)
	}
	// Forward the child trace context so the upstream engine joins the trace.
	if tp := outgoingTraceparent(r.Context()); tp != "" {
		outReq.Header.Set("traceparent", tp)
	}

	resp, err := proxyClient.Do(outReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "backend request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

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
