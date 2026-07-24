"""OpenAI Python SDK conformance check against a running Mainspring server.

Exercises the surface the ecosystem most often gets wrong: streaming frames,
tool_call function.arguments being a JSON *string*, embeddings, models listing,
and 404 error mapping. Exits non-zero on the first failure.
"""
import json
import sys

import openai
from openai import OpenAI

BASE = "http://localhost:11500/v1"
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

if failed:
    sys.exit(1)
print("ALL PASS (python)")
