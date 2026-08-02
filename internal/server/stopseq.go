package server

import "strings"

// stopWatch owns stop-sequence detection for the Anthropic /v1/messages path.
//
// Anthropic reports *which* sequence ended a generation — stop_reason
// "stop_sequence" plus the sequence itself. OpenAI reports only finish_reason
// "stop", which it also uses for a natural end of turn, so the two cases are
// indistinguishable downstream. Worse, every engine Mainspring drives (llama.cpp,
// Ollama, MLX) erases the matched sequence from the text before returning it, so
// once the stop set is forwarded upstream the hit is *unrecoverable*: the reply is
// byte-identical to one that simply ended.
//
// Mainspring therefore does not forward the stop set to the engine and matches the
// sequences itself. That is the only way to answer truthfully, and it is why
// ownedStopBody exists.
//
// feed is incremental because a sequence can straddle two SSE deltas: any suffix
// of the text so far that is a proper prefix of some sequence is held back until
// the next fragment either completes the match or rules it out. Held-back text is
// never lost — flush returns it when the stream ends without a match.
type stopWatch struct {
	seqs []string
	pend string // withheld because it may yet turn out to start a sequence
	hit  string // the sequence that matched; "" until one does
}

// newStopWatch returns a watch for seqs, or nil when there is nothing to watch
// for. A nil *stopWatch is usable: every method below is nil-safe and behaves as
// "no stop sequences", so callers on the no-stop-sequence path need no branch.
func newStopWatch(seqs []string) *stopWatch {
	usable := make([]string, 0, len(seqs))
	for _, s := range seqs {
		if s != "" {
			usable = append(usable, s)
		}
	}
	if len(usable) == 0 {
		return nil
	}
	return &stopWatch{seqs: usable}
}

// feed consumes the next text fragment and returns the part of it that is safe to
// emit, plus whether a stop sequence matched. Once one has matched, feed emits
// nothing further: the generation is over as far as the caller is concerned.
func (sw *stopWatch) feed(chunk string) (emit string, matched bool) {
	if sw == nil {
		return chunk, false
	}
	if sw.hit != "" {
		return "", true
	}
	buf := sw.pend + chunk

	// Earliest match wins: with several sequences in flight, the one that ends the
	// generation is whichever the model reached first. Ties go to the sequence the
	// caller listed first.
	at, seq := -1, ""
	for _, s := range sw.seqs {
		if i := strings.Index(buf, s); i >= 0 && (at < 0 || i < at) {
			at, seq = i, s
		}
	}
	if at >= 0 {
		sw.hit, sw.pend = seq, ""
		return buf[:at], true
	}

	// No match. Hold back the longest suffix that could still become one.
	keep := 0
	for _, s := range sw.seqs {
		for n := min(len(s)-1, len(buf)); n > keep; n-- {
			if strings.HasSuffix(buf, s[:n]) {
				keep = n
				break
			}
		}
	}
	sw.pend = buf[len(buf)-keep:]
	return buf[:len(buf)-keep], false
}

// flush returns any text held back as a possible partial match. Call it when the
// stream ends without a hit — otherwise the tail would be silently dropped.
func (sw *stopWatch) flush() string {
	if sw == nil {
		return ""
	}
	out := sw.pend
	sw.pend = ""
	return out
}

// hitSeq is the sequence that ended the generation, or "" if none did.
func (sw *stopWatch) hitSeq() string {
	if sw == nil {
		return ""
	}
	return sw.hit
}

// stoppedPromptTokens repairs the input-token count when Mainspring's own stop
// cut the stream before the upstream's usage chunk could arrive. Reporting
// input_tokens: 0 would be a visible falsehood in a field callers budget against,
// and it would be one we caused. The character estimate is used rather than the
// tokenizer so no engine round-trip is added to the tail of every stopped
// request; the reply is already marked inexact, which is what says so.
//
// It deliberately does nothing when the stream ended on its own — an engine that
// simply never sends usage is a pre-existing condition, not ours to paper over.
func stoppedPromptTokens(prompt int64, sw *stopWatch, oaiBody []byte) int64 {
	if prompt > 0 || sw.hitSeq() == "" {
		return prompt
	}
	return int64(estimatePromptTokens(oaiBody))
}

// stopReasonFor maps the upstream finish_reason to an Anthropic stop_reason,
// except when Mainspring's own stop watch ended the generation — a case
// finish_reason cannot express.
func stopReasonFor(finish, hit string) string {
	if hit != "" {
		return "stop_sequence"
	}
	return mapStopReason(finish)
}

// stopSequenceField renders the Anthropic stop_sequence field: the matched
// sequence, or JSON null when nothing matched.
func stopSequenceField(hit string) any {
	if hit == "" {
		return nil
	}
	return hit
}
