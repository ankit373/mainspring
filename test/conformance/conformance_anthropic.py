"""Anthropic Python SDK conformance check against a running Mainspring server.

Mainspring implements Anthropic /v1/messages by translating to and from an
OpenAI chat-completions upstream, so there are two things to prove and the
client response only shows one of them:

  * what the caller SEES  — message shape, content blocks, stop_reason, usage,
    the SSE event sequence, and the errors;
  * what the upstream GOT — the system prompt, top_k, stop, image parts and
    role:tool messages the translation produced. The conformance fakeserver
    replies with the exact OpenAI body it received when the prompt carries
    `[[echo]]`, which is how the request half is asserted here.

The failure cases are the point as much as the happy path: an upstream 5xx must
surface as an error rather than a well-formed empty message, and a stream that
ends abnormally must carry a terminal `error` event and no fabricated
message_stop. Exits non-zero on the first failure.
"""
import base64
import json
import sys

import anthropic
from anthropic import Anthropic

BASE = "http://localhost:11500"
MODEL = "mock-model"
client = Anthropic(base_url=BASE, api_key="conformance")

TOOLS = [{
    "name": "get_weather",
    "description": "Look up the weather",
    "input_schema": {
        "type": "object",
        "properties": {"location": {"type": "string"}},
        "required": ["location"],
    },
}]

# A one-pixel-ish PNG header is enough: the assertion is about how the block is
# translated, not about decoding an image.
PNG_B64 = base64.b64encode(bytes.fromhex("89504e470d0a1a0a")).decode()

failed = False


def check(name, cond):
    global failed
    print(("PASS: " if cond else "FAIL: ") + name)
    failed = failed or not cond


def fail(name, detail):
    global failed
    print("FAIL: %s (%s)" % (name, detail))
    failed = True


def echoed(**kw):
    """Run a request the fakeserver echoes, and return the upstream OpenAI body."""
    msgs = kw.pop("messages", [{"role": "user", "content": "[[echo]]"}])
    return json.loads(client.messages.create(
        model=MODEL, max_tokens=64, messages=msgs, **kw).content[0].text)


def err_code(exc):
    """Pull Mainspring's stable error code out of an SDK exception body."""
    body = getattr(exc, "body", None)
    if isinstance(body, dict) and isinstance(body.get("error"), dict):
        return body["error"].get("code")
    return None


# ── 1. text: string content ───────────────────────────────────────────────────
r = client.messages.create(model=MODEL, max_tokens=64,
                           messages=[{"role": "user", "content": "hi"}])
check("message has type/role", r.type == "message" and r.role == "assistant")
check("message echoes the requested model", r.model == MODEL)
check("message has a text block", r.content[0].type == "text" and bool(r.content[0].text))
check("stop_reason maps to end_turn", r.stop_reason == "end_turn")
check("usage reports input and output tokens",
      r.usage.input_tokens > 0 and r.usage.output_tokens > 0)

# ── 2. block content + system + sampling knobs reach the upstream ─────────────
up = echoed(
    system="Be terse.",
    temperature=0.25, top_p=0.9, top_k=7,
    messages=[{"role": "user", "content": [{"type": "text", "text": "[[echo]] hello"}]}],
)
check("system prompt becomes a leading system message",
      up["messages"][0] == {"role": "system", "content": "Be terse."})
check("text blocks are carried through", up["messages"][1]["content"] == "[[echo]] hello")
check("max_tokens is forwarded", up.get("max_tokens") == 64)
check("temperature is forwarded", up.get("temperature") == 0.25)
check("top_p is forwarded", up.get("top_p") == 0.9)
check("top_k is forwarded (not silently dropped)", up.get("top_k") == 7)

# ── 2b. stop sequences ────────────────────────────────────────────────────────
# Anthropic names the sequence that ended a generation. OpenAI's finish_reason
# cannot express it, and every engine erases the match before returning, so the
# stop set is deliberately NOT forwarded: Mainspring matches it itself. A
# non-streaming request is streamed upstream so it can be cut off at the match
# instead of running on to max_tokens.
up = echoed(stop_sequences=["END", "STOP"])
check("stop sequences are not forwarded to the engine", "stop" not in up)
check("a non-streaming request with stop sequences is streamed upstream",
      up.get("stream") is True)

# The fakeserver's canned reply is "Hello from fakeserver ." — stopping on
# "fakeserver" must cut the text there and name the sequence that did it.
r = client.messages.create(model=MODEL, max_tokens=64, stop_sequences=["fakeserver"],
                           messages=[{"role": "user", "content": "hi"}])
check("a stop sequence reports stop_reason stop_sequence", r.stop_reason == "stop_sequence")
check("the stop sequence that hit is named", r.stop_sequence == "fakeserver")
check("text is cut at the stop sequence", r.content[0].text == "Hello from ")
check("a stopped reply still reports input tokens", r.usage.input_tokens > 0)

evts = list(client.messages.create(model=MODEL, max_tokens=64, stream=True,
                                   stop_sequences=["fakeserver"],
                                   messages=[{"role": "user", "content": "hi"}]))
stext = "".join(e.delta.text for e in evts
                if e.type == "content_block_delta" and e.delta.type == "text_delta")
sdelta = [e for e in evts if e.type == "message_delta"]
check("streamed text is cut at the stop sequence", stext == "Hello from ")
check("a stopped stream still ends with message_stop", evts[-1].type == "message_stop")
check("a stopped stream is not reported as an error",
      all(e.type != "error" for e in evts))
check("streamed stop sequence reports stop_reason stop_sequence",
      bool(sdelta) and sdelta[0].delta.stop_reason == "stop_sequence")
check("streamed stop sequence is named",
      bool(sdelta) and sdelta[0].delta.stop_sequence == "fakeserver")

# A sequence the model never produces must leave the natural ending alone.
r = client.messages.create(model=MODEL, max_tokens=64, stop_sequences=["[[never]]"],
                           messages=[{"role": "user", "content": "hi"}])
check("an unmatched stop sequence leaves stop_reason alone", r.stop_reason == "end_turn")
check("an unmatched stop sequence reports stop_sequence null", r.stop_sequence is None)
check("an unmatched stop sequence loses no text", r.content[0].text == "Hello from fakeserver . ")

up = echoed(system=[{"type": "text", "text": "Block system."}])
check("system given as text blocks also becomes a system message",
      up["messages"][0] == {"role": "system", "content": "Block system."})

# ── 3. image blocks ───────────────────────────────────────────────────────────
up = echoed(messages=[{"role": "user", "content": [
    {"type": "text", "text": "[[echo]] describe"},
    {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": PNG_B64}},
]}])
parts = up["messages"][0]["content"]
check("image turn keeps text and image parts in order",
      isinstance(parts, list) and [p["type"] for p in parts] == ["text", "image_url"])
check("base64 image source becomes a data: URI",
      parts[1]["image_url"]["url"] == "data:image/png;base64," + PNG_B64)

# ── 4. streaming text ─────────────────────────────────────────────────────────
events = list(client.messages.create(model=MODEL, max_tokens=64, stream=True,
                                     messages=[{"role": "user", "content": "hi"}]))
kinds = [e.type for e in events]
text = "".join(e.delta.text for e in events
               if e.type == "content_block_delta" and e.delta.type == "text_delta")
check("stream opens with message_start", kinds[0] == "message_start")
check("stream ends with message_stop", kinds[-1] == "message_stop")
check("stream frames a content block",
      "content_block_start" in kinds and "content_block_stop" in kinds)
check("stream produced text", len(text) > 0)
deltas = [e for e in events if e.type == "message_delta"]
check("stream has exactly one message_delta", len(deltas) == 1)
if deltas:
    usage = deltas[0].usage
    # 7/13 are the fakeserver's include_usage numbers; 4 is the text-frame count.
    # Reporting 4 would mean the upstream usage chunk was ignored, and reporting
    # 0 input_tokens would mean the terminal delta never carried the prompt cost.
    check("final message_delta reports input_tokens",
          getattr(usage, "input_tokens", None) == 7)
    check("final message_delta reports exact output_tokens, not a frame count",
          usage.output_tokens == 13)
    check("final message_delta carries stop_reason", deltas[0].delta.stop_reason == "end_turn")

with client.messages.stream(model=MODEL, max_tokens=64,
                            messages=[{"role": "user", "content": "hi"}]) as s:
    final = s.get_final_message()
check("SDK stream accumulator rebuilds the message", final.content[0].text == text)
check("accumulated message carries usage", final.usage.input_tokens == 7)

# ── 5. tool use: non-streaming ────────────────────────────────────────────────
r = client.messages.create(model=MODEL, max_tokens=64, tools=TOOLS,
                           tool_choice={"type": "any"},
                           messages=[{"role": "user", "content": "weather in SF?"}])
check("tool call maps to stop_reason tool_use", r.stop_reason == "tool_use")
blocks = [b for b in r.content if b.type == "tool_use"]
check("response carries a tool_use block", len(blocks) == 1)
tu = blocks[0]
# The mirror of the OpenAI trap: OpenAI arguments are a JSON *string*, Anthropic
# tool_use.input must be a decoded object.
check("tool_use.input is an object, not a JSON string", isinstance(tu.input, dict))
check("tool_use.input decodes the arguments", tu.input.get("location") == "San Francisco")
check("tool_use carries id and name", bool(tu.id) and tu.name == "get_weather")

# ── 6. tool definitions and tool_choice translation ───────────────────────────
up = echoed(tools=TOOLS, tool_choice={"type": "any"})
check("tools become OpenAI function tools",
      up["tools"][0]["type"] == "function"
      and up["tools"][0]["function"]["name"] == "get_weather")
check("input_schema becomes function.parameters",
      up["tools"][0]["function"]["parameters"] == TOOLS[0]["input_schema"])
check("tool_choice any becomes required", up.get("tool_choice") == "required")
up = echoed(tools=TOOLS, tool_choice={"type": "auto"})
check("tool_choice auto stays auto", up.get("tool_choice") == "auto")
up = echoed(tools=TOOLS, tool_choice={"type": "tool", "name": "get_weather"})
check("tool_choice tool names the function",
      up.get("tool_choice") == {"type": "function", "function": {"name": "get_weather"}})

# ── 7. tool round trip through the SDK's own types ────────────────────────────
up = echoed(tools=TOOLS, messages=[
    {"role": "user", "content": "weather in SF?"},
    {"role": "assistant", "content": [
        {"type": "tool_use", "id": tu.id, "name": tu.name, "input": tu.input}]},
    {"role": "user", "content": [
        {"type": "tool_result", "tool_use_id": tu.id, "content": "72F and sunny"},
        {"type": "text", "text": "[[echo]] thanks"}]},
])
roles = [m["role"] for m in up["messages"]]
check("tool round trip preserves turn order", roles == ["user", "assistant", "tool", "user"])
call = up["messages"][1]["tool_calls"][0]
check("assistant tool_use becomes an OpenAI tool_call",
      call["id"] == tu.id and call["function"]["name"] == "get_weather")
check("tool_call arguments are re-encoded as a JSON string",
      isinstance(call["function"]["arguments"], str)
      and json.loads(call["function"]["arguments"]) == tu.input)
check("tool_result becomes a role:tool message",
      up["messages"][2] == {"role": "tool", "tool_call_id": tu.id, "content": "72F and sunny"})

# ── 8. streaming tool use ─────────────────────────────────────────────────────
events = list(client.messages.create(model=MODEL, max_tokens=64, tools=TOOLS, stream=True,
                                     messages=[{"role": "user", "content": "weather?"}]))
starts = [e for e in events if e.type == "content_block_start"]
check("text and tool_use get separate content blocks",
      [b.content_block.type for b in starts] == ["text", "tool_use"])
check("content block indices are distinct and ordered",
      [b.index for b in starts] == [0, 1])
check("tool_use block start names the tool",
      starts[1].content_block.name == "get_weather" and bool(starts[1].content_block.id))
partials = [e.delta.partial_json for e in events
            if e.type == "content_block_delta" and e.delta.type == "input_json_delta"]
check("tool arguments stream as input_json_delta", len(partials) >= 2)
check("input_json_delta fragments reassemble into the arguments",
      json.loads("".join(partials)) == {"location": "SF"})

with client.messages.stream(model=MODEL, max_tokens=64, tools=TOOLS,
                            messages=[{"role": "user", "content": "weather?"}]) as s:
    final = s.get_final_message()
tool_blocks = [b for b in final.content if b.type == "tool_use"]
check("accumulator rebuilds the tool_use block from the stream",
      len(tool_blocks) == 1 and tool_blocks[0].input == {"location": "SF"})
check("streamed tool call reports stop_reason tool_use", final.stop_reason == "tool_use")

# ── 9. upstream 5xx must not decode into an empty success ─────────────────────
try:
    r = client.messages.create(model=MODEL, max_tokens=64,
                               messages=[{"role": "user", "content": "[[status:500]]"}])
    fail("upstream 5xx raises instead of returning a message",
         "returned %r" % (r.model_dump(),))
except anthropic.APIStatusError as e:
    check("upstream 5xx raises an API error", True)
    check("upstream 5xx becomes 502", e.status_code == 502)
    check("upstream 5xx carries the upstream_error code", err_code(e) == "upstream_error")

# A 4xx is the caller's fault, so the status is preserved rather than masked.
try:
    client.messages.create(model=MODEL, max_tokens=64,
                           messages=[{"role": "user", "content": "[[status:400]]"}])
    fail("upstream 4xx keeps its own status", "no error raised")
except anthropic.BadRequestError as e:
    check("upstream 4xx keeps its own status", e.status_code == 400)

# The stream variant must fail before a single SSE byte, while the status is
# still correctable.
try:
    with client.messages.stream(model=MODEL, max_tokens=64,
                                messages=[{"role": "user", "content": "[[status:500]]"}]) as s:
        for _ in s:
            pass
    fail("upstream 5xx on a stream raises", "stream completed")
except anthropic.APIStatusError as e:
    check("upstream 5xx on a stream raises before any event", e.status_code == 502)

# ── 10. a stream that ends abnormally must say so ─────────────────────────────
seen, got = [], None
try:
    for e in client.messages.create(model=MODEL, max_tokens=64, stream=True,
                                    messages=[{"role": "user", "content": "[[truncate]]"}]):
        seen.append(e.type)
except anthropic.APIError as e:
    got = e
if got is None:
    fail("truncated stream raises", "stream completed cleanly: %s" % seen)
else:
    check("truncated stream raises an API error", True)
    check("truncated stream carries the upstream_error code", err_code(got) == "upstream_error")
    check("truncated stream delivered content before failing",
          "content_block_delta" in seen)
    check("truncated stream closes the open content block", seen[-1] == "content_block_stop")
    check("truncated stream fabricates no message_stop", "message_stop" not in seen)
    check("truncated stream fabricates no message_delta", "message_delta" not in seen)

# The high-level helper must not hand back a final message either.
try:
    with client.messages.stream(model=MODEL, max_tokens=64,
                                messages=[{"role": "user", "content": "[[truncate]]"}]) as s:
        m = s.get_final_message()
    fail("truncated stream yields no final message", "returned %r" % (m.stop_reason,))
except anthropic.APIError:
    check("truncated stream yields no final message", True)

# ── 11. unknown model ─────────────────────────────────────────────────────────
try:
    client.messages.create(model="does-not-exist", max_tokens=8,
                           messages=[{"role": "user", "content": "x"}])
    fail("unknown model raises NotFoundError", "no error raised")
except anthropic.NotFoundError:
    check("unknown model raises NotFoundError", True)
except Exception as ex:  # noqa: BLE001
    fail("unknown model raises NotFoundError", "raised " + type(ex).__name__)

if failed:
    sys.exit(1)
print("ALL PASS (anthropic python)")
