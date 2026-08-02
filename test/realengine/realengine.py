"""Drive Mainspring against a REAL engine, with the real OpenAI and Anthropic SDKs.

Why this exists
---------------
Every other test in this repo runs against `test/conformance/fakeserver`, which
this project wrote. That makes the *client* side genuinely verified — the
conformance suites drive the real SDKs — but the *engine* side is always a
fixture that behaves exactly as we assumed it would. This suite checks the
assumptions themselves against an engine nobody here controls.

The first run of it found #197 (a typo'd config key silently disabling the
context guardrail) and #198 (`n:3` silently returning one choice), and confirmed
the premise #192's whole design rests on: that the engine erases a matched stop
sequence before returning, making a stop-sequence hit indistinguishable from a
natural ending unless Mainspring matches the sequences itself.

Writing assertions that survive a model swap
--------------------------------------------
The temptation is to prompt for exact output ("reply with alpha###beta") and
assert on it. That tests the *model*, not the server: a small model ignores the
instruction and the suite goes red for no reason. So every assertion here is
about mechanism, not content, and stop sequences are chosen to be things any
non-empty multi-word reply must contain — a space. What is asserted is that the
text stops before it and that the stop is reported, never what the words were.
"""
import json
import os
import sys
import urllib.error
import urllib.request

import anthropic
from openai import OpenAI

MS = os.environ.get("MAINSPRING_URL", "http://127.0.0.1:11501")
ENGINE = os.environ.get("ENGINE_URL", "http://127.0.0.1:11434")
MODEL = os.environ.get("REAL_MODEL", "qwen2.5:0.5b")
TIMEOUT = float(os.environ.get("REAL_TIMEOUT", "180"))

oai = OpenAI(base_url=MS + "/v1", api_key="realengine", timeout=TIMEOUT)
ant = anthropic.Anthropic(base_url=MS, api_key="realengine", timeout=TIMEOUT)

# A prompt that cannot answer in one word, so a space is guaranteed to appear and
# a space is therefore usable as a stop sequence no model can dodge.
MULTIWORD = "Count from one to ten, separated by spaces."

ran, failed = 0, []


def check(name, ok, detail=""):
    """Record one check. `detail` prints on pass and fail alike, so it must state
    an observed fact — never explain a failure that may not have happened."""
    global ran
    ran += 1
    print(("PASS: " if ok else "FAIL: ") + name + (("  — " + detail) if detail else ""))
    sys.stdout.flush()
    if not ok:
        failed.append((name, detail))


def post(url, payload, want_headers=False):
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(),
        headers={"content-type": "application/json"})
    with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
        body = json.loads(r.read())
        if want_headers:
            return body, {k.lower(): v for k, v in r.getheaders()}
        return body


def section(n, title):
    print()
    print("=" * 72)
    print("%s. %s" % (n, title))
    print("=" * 72)


# ── 1. the upstream assumption Mainspring's stop handling depends on ──────────
section(1, "Does the engine itself erase a matched stop sequence?")
direct = post(ENGINE + "/v1/chat/completions", {
    "model": MODEL, "temperature": 0, "max_tokens": 40, "stop": [" "],
    "messages": [{"role": "user", "content": MULTIWORD}]})
dtext = direct["choices"][0]["message"]["content"]
dfinish = direct["choices"][0]["finish_reason"]
print("   engine returned %r  finish_reason=%r" % (dtext, dfinish))
# If this ever fails, the sequence survived and #192's premise does not hold for
# this engine — Mainspring would be matching a stop the engine also matched.
check("the engine erases the matched stop sequence", " " not in dtext,
      "returned %r" % dtext)
check("the engine reports the hit as finish_reason 'stop', indistinguishable "
      "from a natural end", dfinish == "stop", "finish_reason=%r" % dfinish)

# ── 2. Anthropic stop_sequences end to end ───────────────────────────────────
section(2, "stop_sequences through /v1/messages")
m = ant.messages.create(model=MODEL, max_tokens=40, temperature=0, stop_sequences=[" "],
                        messages=[{"role": "user", "content": MULTIWORD}])
mtext = "".join(b.text for b in m.content if b.type == "text")
print("   mainspring returned %r  stop_reason=%r stop_sequence=%r"
      % (mtext, m.stop_reason, m.stop_sequence))
check("a real stop-sequence hit reports stop_reason 'stop_sequence'",
      m.stop_reason == "stop_sequence", "got %r" % m.stop_reason)
check("the sequence that hit is named", m.stop_sequence == " ",
      "got %r" % m.stop_sequence)
check("the text stops before the sequence", " " not in mtext, "got %r" % mtext)
# Cutting the stream at the match also cuts the upstream usage chunk, so this is
# the estimate Mainspring fills in rather than reporting a zero it caused.
check("a stopped reply still reports input tokens", m.usage.input_tokens > 0,
      "input_tokens=%d" % m.usage.input_tokens)

evs = list(ant.messages.create(model=MODEL, max_tokens=40, temperature=0, stream=True,
                               stop_sequences=[" "],
                               messages=[{"role": "user", "content": MULTIWORD}]))
stext = "".join(e.delta.text for e in evs
                if e.type == "content_block_delta" and e.delta.type == "text_delta")
sd = [e for e in evs if e.type == "message_delta"]
check("a streamed hit reports stop_reason 'stop_sequence'",
      bool(sd) and sd[0].delta.stop_reason == "stop_sequence",
      "got %r" % (sd[0].delta.stop_reason if sd else None))
check("the streamed text stops before the sequence", " " not in stext, "got %r" % stext)
check("a stopped stream still ends with message_stop", evs[-1].type == "message_stop",
      "last event=%r" % evs[-1].type)
check("our own stop is not reported as a broken stream",
      all(e.type != "error" for e in evs))

# ── 3. token accounting, which the whole cost and budget layer rests on ──────
section(3, "Token accounting against a real engine")
r = oai.chat.completions.create(model=MODEL, max_tokens=20, temperature=0,
                                messages=[{"role": "user", "content": "Say hi."}])
check("non-streaming reports prompt and completion tokens",
      r.usage.prompt_tokens > 0 and r.usage.completion_tokens > 0,
      "prompt=%d completion=%d" % (r.usage.prompt_tokens, r.usage.completion_tokens))

chunks = list(oai.chat.completions.create(
    model=MODEL, max_tokens=20, temperature=0, stream=True,
    stream_options={"include_usage": True},
    messages=[{"role": "user", "content": "Say hi."}]))
usage = [c for c in chunks if c.usage]
# No usage chunk means streamed accounting on this engine is an estimate rather
# than exact — reported honestly by Mainspring either way, but worth knowing.
check("the engine honours stream_options.include_usage", bool(usage),
      "usage chunks=%d" % len(usage))

am = ant.messages.create(model=MODEL, max_tokens=20, temperature=0,
                         messages=[{"role": "user", "content": "Say hi."}])
check("/v1/messages reports real token counts",
      am.usage.input_tokens > 0 and am.usage.output_tokens > 0,
      "in=%d out=%d" % (am.usage.input_tokens, am.usage.output_tokens))

adelta = [e for e in ant.messages.create(model=MODEL, max_tokens=20, temperature=0,
                                         stream=True,
                                         messages=[{"role": "user", "content": "Say hi."}])
          if e.type == "message_delta"]
check("streamed /v1/messages reports real input tokens",
      bool(adelta) and adelta[0].usage.input_tokens > 0,
      "in=%s" % (adelta[0].usage.input_tokens if adelta else None))

# ── 4. fail-loud surfaces must describe the engine that actually ran ─────────
section(4, "Fail-loud surfaces name what actually ran")
_, hdrs = post(MS + "/v1/chat/completions", {
    "model": MODEL, "max_tokens": 8, "temperature": 0,
    "messages": [{"role": "user", "content": "hi"}]}, want_headers=True)
check("X-Mainspring-Backend names a real backend",
      bool(hdrs.get("x-mainspring-backend")),
      "got %r" % hdrs.get("x-mainspring-backend"))
check("X-Mainspring-Device says where it ran",
      bool(hdrs.get("x-mainspring-device")), "got %r" % hdrs.get("x-mainspring-device"))

tok = post(MS + "/v1/tokenize", {"model": MODEL, "input": "hello there"})
check("tokenize answers", tok["tokens"] > 0, "tokens=%d" % tok["tokens"])
# An adopted daemon exposes no tokenizer, so the count is a character estimate and
# must say so rather than passing itself off as the model's own count.
check("tokenize is honestly flagged inexact on an engine with no tokenizer",
      tok["exact"] is False, "exact=%r" % tok["exact"])

ct = ant.messages.count_tokens(model=MODEL,
                               messages=[{"role": "user", "content": "hello there"}])
check("count_tokens answers without loading anything new", ct.input_tokens > 0,
      "input_tokens=%d" % ct.input_tokens)

# ── 5. the error taxonomy against a real engine ──────────────────────────────
section(5, "Errors stay inside the taxonomy")
try:
    ant.messages.create(model="model-that-does-not-exist", max_tokens=8,
                        messages=[{"role": "user", "content": "x"}])
    check("an unknown model is a taxonomy 404", False, "no error raised")
except anthropic.NotFoundError as e:
    body = getattr(e, "body", None) or {}
    code = body.get("error", {}).get("code") if isinstance(body, dict) else None
    check("an unknown model is a taxonomy 404 with model_not_found",
          code == "model_not_found", "code=%r" % code)

try:
    urllib.request.urlopen(MS + "/v1/no-such-endpoint", timeout=TIMEOUT)
    check("an unrouted path is a taxonomy 404", False, "no error raised")
except urllib.error.HTTPError as e:
    body = json.loads(e.read() or b"{}")
    code = body.get("error", {}).get("code")
    check("an unrouted path is a taxonomy 404 with route_not_found",
          e.code == 404 and code == "route_not_found",
          "status=%d code=%r" % (e.code, code))

print()
print("=" * 72)
print("%d checks, %d failed" % (ran, len(failed)))
for name, detail in failed:
    print("  FAIL %s — %s" % (name, detail))
print("=" * 72)
if failed:
    sys.exit(1)
print("ALL PASS (real engine: %s)" % MODEL)
