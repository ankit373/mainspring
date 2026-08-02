// OpenAI JS SDK conformance check against a running Mainspring server.
// Mirrors the Python suite: streaming, tool_call arguments as a JSON string,
// embeddings, models listing, 404 mapping, and an `n` the engine quietly refused.
import OpenAI from "openai";

const BASE = "http://localhost:11500/v1";
const AUTH = { authorization: "Bearer conformance", "content-type": "application/json" };
const client = new OpenAI({ baseURL: BASE, apiKey: "conformance" });

let failed = false;
function check(name, cond) {
  console.log((cond ? "PASS: " : "FAIL: ") + name);
  if (!cond) failed = true;
}

const models = (await client.models.list()).data.map((m) => m.id);
check("models.list contains mock-model", models.includes("mock-model"));

const r = await client.chat.completions.create({
  model: "mock-model",
  messages: [{ role: "user", content: "hi" }],
});
check("chat completion has content", Boolean(r.choices[0].message.content));

const stream = await client.chat.completions.create({
  model: "mock-model",
  messages: [{ role: "user", content: "hi" }],
  stream: true,
});
let text = "";
for await (const c of stream) text += c.choices[0]?.delta?.content ?? "";
check("stream produced content", text.length > 0);

const t = await client.chat.completions.create({
  model: "mock-model",
  messages: [{ role: "user", content: "weather?" }],
  tools: [{
    type: "function",
    function: {
      name: "get_weather",
      parameters: { type: "object", properties: { location: { type: "string" } } },
    },
  }],
});
const args = t.choices[0].message.tool_calls[0].function.arguments;
check("tool_call arguments is a string", typeof args === "string");
check("tool_call arguments parse as JSON", typeof JSON.parse(args) === "object");

try {
  await client.chat.completions.create({ model: "nope", messages: [{ role: "user", content: "x" }] });
  check("unknown model raises 404", false);
} catch (e) {
  check("unknown model raises 404", e.status === 404);
}

// `n` > 1 is verified against what came back, not assumed. The fakeserver ignores
// `n` unless asked to honour it — the two engines Mainspring must tell apart
// without a per-backend table.
async function chatRaw(content, n) {
  const { data, response } = await client.chat.completions
    .create({ model: "mock-model", ...(n ? { n } : {}), messages: [{ role: "user", content }] })
    .withResponse();
  return { data, warning: response.headers.get("x-mainspring-warning") ?? "" };
}

const ignored = await chatRaw("three please", 3);
check("engine that ignores n returns one choice", ignored.data.choices.length === 1);
check("an ignored n is reported on X-Mainspring-Warning", ignored.warning.includes("n=3 requested"));

const honoured = await chatRaw("[[honour-n]] three please", 3);
check("engine that honours n returns three choices", honoured.data.choices.length === 3);
check("a honoured n is not warned about", !honoured.warning.includes("the engine returned"));

const plain = await chatRaw("hi");
check("an n-less request carries no choice warning", !plain.warning.includes("the engine returned"));

// Streaming: the verdict only exists at end-of-stream, where the headers are long
// gone, so it arrives in-band as an SSE comment. The SDK must still parse the
// stream (the event-stream grammar defines comments as inert).
async function streamRaw(content) {
  const res = await fetch(`${BASE}/chat/completions`, {
    method: "POST",
    headers: AUTH,
    body: JSON.stringify({
      model: "mock-model", n: 3, stream: true,
      messages: [{ role: "user", content }],
    }),
  });
  return await res.text();
}

const nStream = await client.chat.completions.create({
  model: "mock-model", n: 3, stream: true,
  messages: [{ role: "user", content: "three" }],
});
let nChunks = 0;
for await (const _c of nStream) nChunks += 1;
check("SDK parses a stream carrying the in-band warning", nChunks > 0);

const ignoredStream = await streamRaw("three");
check("an ignored n is reported in-band on the stream",
  ignoredStream.includes(": X-Mainspring-Warning: n=3 requested"));

const honouredStream = await streamRaw("[[honour-n]] three");
check("a honoured n leaves the stream unannotated", !honouredStream.includes("X-Mainspring-Warning"));
check("a honoured n streams three distinct choice indices",
  [0, 1, 2].every((i) => honouredStream.includes(`"index":${i}`)));

if (failed) process.exit(1);
console.log("ALL PASS (node)");
