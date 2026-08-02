"""OpenAI Python SDK conformance check against a running Mainspring server.

Exercises the surface the ecosystem most often gets wrong: streaming frames,
tool_call function.arguments being a JSON *string*, embeddings, models listing,
404 error mapping, and an `n` the engine quietly refused. Exits non-zero on the
first failure.
"""
import json
import sys

import httpx
import openai
from openai import OpenAI

BASE = "http://localhost:11500/v1"
AUTH = {"authorization": "Bearer conformance"}
client = OpenAI(base_url=BASE, api_key="conformance")

failed = False


def check(name, cond):
    global failed
    print(("PASS: " if cond else "FAIL: ") + name)
    failed = failed or not cond


# 1. models.list
models = [m.id for m in client.models.list().data]
check("models.list contains mock-model", "mock-model" in models)

# 2. non-streaming chat completion + usage
r = client.chat.completions.create(model="mock-model", messages=[{"role": "user", "content": "hi"}])
check("chat completion has content", bool(r.choices[0].message.content))
check("chat completion reports usage", r.usage is not None and r.usage.total_tokens > 0)

# 3. streaming
chunks = list(client.chat.completions.create(
    model="mock-model", messages=[{"role": "user", "content": "hi"}], stream=True))
text = "".join((c.choices[0].delta.content or "") for c in chunks if c.choices)
check("stream produced content", len(text) > 0)
check("stream has a finish_reason", any(c.choices and c.choices[0].finish_reason for c in chunks))

# 4. tool_call arguments MUST be a JSON string (the #1 compat trap)
r = client.chat.completions.create(
    model="mock-model",
    messages=[{"role": "user", "content": "weather in SF?"}],
    tools=[{"type": "function", "function": {
        "name": "get_weather",
        "parameters": {"type": "object", "properties": {"location": {"type": "string"}}},
    }}],
)
tc = r.choices[0].message.tool_calls[0]
check("tool_call.function.arguments is a str", isinstance(tc.function.arguments, str))
check("tool_call arguments parse as JSON object", isinstance(json.loads(tc.function.arguments), dict))

# 5. embeddings
e = client.embeddings.create(model="mock-model", input="hello")
check("embeddings return a vector", len(e.data[0].embedding) > 0)

# 6. unknown model -> 404 NotFoundError
try:
    client.chat.completions.create(model="does-not-exist", messages=[{"role": "user", "content": "x"}])
    check("unknown model raises NotFoundError", False)
except openai.NotFoundError:
    check("unknown model raises NotFoundError", True)
except Exception as ex:  # noqa: BLE001
    print("FAIL: unknown model raised unexpected", type(ex).__name__)
    failed = True


# 7. `n` > 1 is verified against what came back, not assumed. The fakeserver
#    ignores `n` unless asked to honour it — the two engines Mainspring has to
#    tell apart without a per-backend table.
def chat_raw(**kw):
    r = client.chat.completions.with_raw_response.create(model="mock-model", **kw)
    return r.parse(), r.headers.get("x-mainspring-warning", "")


ignored, warn = chat_raw(n=3, messages=[{"role": "user", "content": "three please"}])
check("engine that ignores n returns one choice", len(ignored.choices) == 1)
check("an ignored n is reported on X-Mainspring-Warning", "n=3 requested" in warn)

honoured, warn = chat_raw(n=3, messages=[{"role": "user", "content": "[[honour-n]] three please"}])
check("engine that honours n returns three choices", len(honoured.choices) == 3)
check("a honoured n is not warned about", "the engine returned" not in warn)

_, warn = chat_raw(messages=[{"role": "user", "content": "hi"}])
check("an n-less request carries no choice warning", "the engine returned" not in warn)


# 8. Streaming: the verdict only exists at end-of-stream, where the headers are
#    long gone, so it arrives in-band as an SSE comment. The SDK must still parse
#    the stream (the event-stream grammar defines comments as inert).
def stream_raw(content, n=3):
    r = httpx.post(BASE + "/chat/completions", headers=AUTH, timeout=30, json={
        "model": "mock-model", "n": n, "stream": True,
        "messages": [{"role": "user", "content": content}]})
    return r.text


chunks = list(client.chat.completions.create(
    model="mock-model", n=3, stream=True, messages=[{"role": "user", "content": "three"}]))
check("SDK parses a stream carrying the in-band warning", len(chunks) > 0)

body = stream_raw("three")
check("an ignored n is reported in-band on the stream",
      ": X-Mainspring-Warning: n=3 requested" in body)

body = stream_raw("[[honour-n]] three")
check("a honoured n leaves the stream unannotated", "X-Mainspring-Warning" not in body)
check("a honoured n streams three distinct choice indices",
      all(f'"index":{i}' in body for i in range(3)))

if failed:
    sys.exit(1)
print("ALL PASS (python)")
