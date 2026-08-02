package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// warningHeader carries every fail-loud warning about what actually ran. More
// than one can be true of a single response, so it composes (see addWarning)
// rather than being overwritten by whoever sets it last.
const warningHeader = "X-Mainspring-Warning"

// choiceBodyCap bounds how much of a non-streaming response is buffered in order
// to count its choices. A larger body streams through uncounted: the check is a
// courtesy to the caller, not a licence to hold an answer in memory.
const choiceBodyCap = 1 << 20 // 1 MiB

// choiceFrameCap bounds the partial SSE line held between reads while counting
// stream indices. A frame that outgrows it stops the count — reported as no
// warning, never as a shortfall — instead of growing without bound.
const choiceFrameCap = 1 << 20 // 1 MiB

// choiceCheck observes how many candidates the engine actually returned for a
// request that asked for more than one.
//
// OpenAI's `n` asks for several independent completions in one call. Every engine
// Mainspring drives accepts the field; the ones that do not implement it answer
// 200 with exactly one choice and say nothing, so a caller that asked for three
// candidates to rank gets one and cannot tell that it was denied.
//
// Mainspring keeps no per-backend table of who honours `n`. Support moves between
// engine versions, and such a table would start lying the moment one of them
// shipped. It counts what came back instead — the same way the effective context
// window and the device are observed rather than declared.
//
// The engine's answer is never altered: `n` is still forwarded, the body is
// forwarded byte-for-byte, and the shortfall is reported alongside it.
//
// A nil *choiceCheck is the common path — no `n`, or n<=1 — and every method is
// nil-safe, so that path takes exactly the route it took before: no response
// parsing, no buffering, no allocation.
type choiceCheck struct {
	want  int          // candidates the caller asked for (always > 1)
	seen  map[int]bool // streaming: distinct choices[].index values seen so far
	pend  []byte       // streaming: trailing partial line carried between reads
	blind bool         // streaming: a frame outgrew choiceFrameCap; count abandoned
}

// newChoiceCheck returns a check for a request that asked for n>1 candidates on
// an endpoint whose response carries a `choices` array, and nil for everything
// else — including /v1/embeddings, where `n` is not part of the API and there is
// nothing to count.
func newChoiceCheck(n *int, path string) *choiceCheck {
	if n == nil || *n <= 1 {
		return nil
	}
	if path != "/v1/chat/completions" && path != "/v1/completions" {
		return nil
	}
	return &choiceCheck{want: *n}
}

// observe prepares the upstream body for counting and returns the reader to
// forward to the client, unchanged in content.
//
// For a non-streaming success it buffers the body (bounded) so the count is known
// *before* the status line goes out — a header cannot be added to a response that
// is already committed — and returns the warning to attach. For a stream it
// returns a reader that counts choice indices as frames pass; that verdict only
// exists at end-of-stream and is reported by emitShortfall.
//
// The returned error is a failed read of the buffered prefix: the caller must
// still commit and then surface it, so a severed body stays marked incomplete.
func (cc *choiceCheck) observe(resp *http.Response) (io.Reader, string, error) {
	if cc == nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Body, "", nil
	}
	if isEventStream(resp.Header.Get("Content-Type")) {
		cc.seen = make(map[int]bool, cc.want)
		return io.TeeReader(resp.Body, cc), "", nil
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, choiceBodyCap+1))
	src := io.MultiReader(bytes.NewReader(head), resp.Body)
	if err != nil || len(head) > choiceBodyCap {
		return src, "", err // unreadable, or too large to count
	}
	return src, cc.shortfall(countChoices(head)), nil
}

// countChoices returns the number of entries in a non-streaming response's
// `choices` array, or -1 when the body does not say — not JSON, or no such field.
// An answer we cannot read is not evidence of a shortfall.
func countChoices(body []byte) int {
	var r struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(body, &r) != nil || r.Choices == nil {
		return -1
	}
	return len(r.Choices)
}

// shortfall renders the warning for `got` choices against what was asked, or ""
// when the engine satisfied the request or the count was unobservable (got < 1).
func (cc *choiceCheck) shortfall(got int) string {
	if cc == nil || got < 1 || got >= cc.want {
		return ""
	}
	noun := "choices"
	if got == 1 {
		noun = "choice"
	}
	return fmt.Sprintf("n=%d requested but the engine returned %d %s", cc.want, got, noun)
}

// Write counts distinct choice indices across SSE frames. It is the io.Writer end
// of a TeeReader, so it sees exactly the bytes forwarded to the client and cannot
// alter them. Frames split across reads are stitched through cc.pend, because a
// 32 KiB read boundary falls wherever it likes.
func (cc *choiceCheck) Write(p []byte) (int, error) {
	if cc.blind {
		return len(p), nil
	}
	cc.pend = append(cc.pend, p...)
	for {
		i := bytes.IndexByte(cc.pend, '\n')
		if i < 0 {
			break
		}
		cc.countFrame(cc.pend[:i])
		cc.pend = cc.pend[i+1:]
	}
	if len(cc.pend) > choiceFrameCap {
		cc.blind, cc.pend = true, nil
	}
	return len(p), nil
}

// countFrame records the choice indices carried by one SSE line. Anything that is
// not an OpenAI data frame — a blank line, a comment, the terminating [DONE] —
// carries no choices and is skipped. The indices are read by decoding the frame
// rather than by scanning for `"index"`, which also appears inside tool_calls.
func (cc *choiceCheck) countFrame(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\r")), []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var f struct {
		Choices []struct {
			Index int `json:"index"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &f) != nil {
		return
	}
	for _, c := range f.Choices {
		cc.seen[c.Index] = true
	}
}

// emitShortfall reports an end-of-stream shortfall in-band and logs it.
//
// A stream's status line left long before the last frame arrived, so a header is
// physically unavailable here. An SSE comment is the one channel the event-stream
// grammar defines as inert: no conforming client parses it as an event, so nothing
// breaks, and it is still in the bytes the caller received. The log line is what
// an operator actually notices.
func (cc *choiceCheck) emitShortfall(w http.ResponseWriter) {
	if cc == nil || cc.blind || cc.seen == nil {
		return
	}
	warn := cc.shortfall(len(cc.seen))
	if warn == "" {
		return
	}
	fmt.Fprintf(w, ": %s: %s\n\n", warningHeader, warn)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	log.Printf("CHOICES %s", warn)
}

// applyShortfall attaches a non-streaming shortfall to the response header and
// logs it. Must be called before the status line is written.
func applyShortfall(h http.Header, warn string) {
	if warn == "" {
		return
	}
	addWarning(h, warn)
	log.Printf("CHOICES %s", warn)
}

// addWarning composes another warning onto the response's warning header instead
// of replacing what is there: a CPU fallback and a short `n` can both be true of
// one response, and a Set would hide whichever was written first.
func addWarning(h http.Header, msg string) {
	ws := []string{msg}
	if cur := h.Get(warningHeader); cur != "" {
		ws = []string{cur, msg}
	}
	h.Set(warningHeader, joinWarnings(ws))
}

// isEventStream reports whether a Content-Type is an SSE stream.
func isEventStream(contentType string) bool {
	return strings.Contains(contentType, "text/event-stream")
}
