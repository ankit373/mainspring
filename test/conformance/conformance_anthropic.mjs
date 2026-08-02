// Anthropic JS SDK conformance check against a running Mainspring server.
//
// The Python suite (conformance_anthropic.py) owns the full surface, including
// what the Anthropic → OpenAI translation puts on the wire. This one covers the
// half a second, independent client implementation can actually disagree about:
// SSE decoding, the stream event union, the accumulator, tool_use input typing,
// and how an abnormal end is surfaced. Duplicating the request-translation
// assertions here would cost runtime and prove nothing new.
import Anthropic from "@anthropic-ai/sdk";

const MODEL = "mock-model";
const client = new Anthropic({ baseURL: "http://localhost:11500", apiKey: "conformance" });
const TOOLS = [{
  name: "get_weather",
  description: "Look up the weather",
  input_schema: { type: "object", properties: { location: { type: "string" } }, required: ["location"] },
}];

let failed = false;
function check(name, cond) {
  console.log((cond ? "PASS: " : "FAIL: ") + name);
  if (!cond) failed = true;
}
function fail(name, detail) {
  console.log(`FAIL: ${name} (${detail})`);
  failed = true;
}
// Mainspring's stable error code, whether the SDK handed us the body or its
// `error` member.
function errCode(e) {
  const b = e?.error ?? {};
  return b.error?.code ?? b.code;
}

// ── 1. non-streaming text ─────────────────────────────────────────────────────
const r = await client.messages.create({
  model: MODEL, max_tokens: 64, messages: [{ role: "user", content: "hi" }],
});
check("message has type/role", r.type === "message" && r.role === "assistant");
check("message echoes the requested model", r.model === MODEL);
check("message has a text block", r.content[0].type === "text" && r.content[0].text.length > 0);
check("stop_reason maps to end_turn", r.stop_reason === "end_turn");
check("usage reports input and output tokens", r.usage.input_tokens > 0 && r.usage.output_tokens > 0);

// ── 2. streaming text ─────────────────────────────────────────────────────────
const events = [];
for await (const e of await client.messages.create({
  model: MODEL, max_tokens: 64, stream: true, messages: [{ role: "user", content: "hi" }],
})) events.push(e);
const kinds = events.map((e) => e.type);
const text = events
  .filter((e) => e.type === "content_block_delta" && e.delta.type === "text_delta")
  .map((e) => e.delta.text).join("");
check("stream opens with message_start", kinds[0] === "message_start");
check("stream ends with message_stop", kinds.at(-1) === "message_stop");
check("stream produced text", text.length > 0);
const delta = events.find((e) => e.type === "message_delta");
// 7/13 are the fakeserver's include_usage numbers; 4 is the text-frame count.
check("final message_delta reports input_tokens", delta?.usage?.input_tokens === 7);
check("final message_delta reports exact output_tokens, not a frame count",
  delta?.usage?.output_tokens === 13);

const accumulated = await client.messages.stream({
  model: MODEL, max_tokens: 64, messages: [{ role: "user", content: "hi" }],
}).finalMessage();
check("SDK stream accumulator rebuilds the message", accumulated.content[0].text === text);

// ── 3. tool use ───────────────────────────────────────────────────────────────
const t = await client.messages.create({
  model: MODEL, max_tokens: 64, tools: TOOLS, tool_choice: { type: "any" },
  messages: [{ role: "user", content: "weather in SF?" }],
});
check("tool call maps to stop_reason tool_use", t.stop_reason === "tool_use");
const tu = t.content.find((b) => b.type === "tool_use");
// The mirror of the OpenAI trap: OpenAI arguments are a JSON *string*, an
// Anthropic tool_use.input must arrive decoded.
check("tool_use.input is an object, not a JSON string",
  tu !== undefined && typeof tu.input === "object" && tu.input !== null);
check("tool_use.input decodes the arguments", tu?.input?.location === "San Francisco");
check("tool_use carries id and name", Boolean(tu?.id) && tu?.name === "get_weather");

// ── 4. streaming tool use ─────────────────────────────────────────────────────
const toolEvents = [];
for await (const e of await client.messages.create({
  model: MODEL, max_tokens: 64, tools: TOOLS, stream: true,
  messages: [{ role: "user", content: "weather?" }],
})) toolEvents.push(e);
const starts = toolEvents.filter((e) => e.type === "content_block_start");
check("text and tool_use get separate content blocks",
  starts.map((e) => e.content_block.type).join(",") === "text,tool_use");
check("content block indices are distinct and ordered",
  starts.map((e) => e.index).join(",") === "0,1");
const partials = toolEvents
  .filter((e) => e.type === "content_block_delta" && e.delta.type === "input_json_delta")
  .map((e) => e.delta.partial_json);
check("tool arguments stream as input_json_delta", partials.length >= 2);
check("input_json_delta fragments reassemble into the arguments",
  JSON.stringify(JSON.parse(partials.join(""))) === JSON.stringify({ location: "SF" }));

const streamedTool = await client.messages.stream({
  model: MODEL, max_tokens: 64, tools: TOOLS, messages: [{ role: "user", content: "weather?" }],
}).finalMessage();
const finalTool = streamedTool.content.find((b) => b.type === "tool_use");
check("accumulator rebuilds the tool_use block from the stream",
  finalTool?.input?.location === "SF");

// ── 5. an upstream 5xx is an error, not an empty message ──────────────────────
try {
  const empty = await client.messages.create({
    model: MODEL, max_tokens: 64, messages: [{ role: "user", content: "[[status:500]]" }],
  });
  fail("upstream 5xx raises instead of returning a message", JSON.stringify(empty));
} catch (e) {
  check("upstream 5xx becomes 502", e.status === 502);
  check("upstream 5xx carries the upstream_error code", errCode(e) === "upstream_error");
}

// ── 6. a stream that ends abnormally must say so ──────────────────────────────
const seen = [];
let thrown;
try {
  for await (const e of await client.messages.create({
    model: MODEL, max_tokens: 64, stream: true, messages: [{ role: "user", content: "[[truncate]]" }],
  })) seen.push(e.type);
} catch (e) {
  thrown = e;
}
if (!thrown) {
  fail("truncated stream raises", `stream completed: ${seen}`);
} else {
  check("truncated stream raises an API error", thrown instanceof Anthropic.APIError);
  check("truncated stream carries the upstream_error code", errCode(thrown) === "upstream_error");
  check("truncated stream delivered content before failing", seen.includes("content_block_delta"));
  check("truncated stream closes the open content block", seen.at(-1) === "content_block_stop");
  check("truncated stream fabricates no message_stop", !seen.includes("message_stop"));
  check("truncated stream fabricates no message_delta", !seen.includes("message_delta"));
}

try {
  const m = await client.messages.stream({
    model: MODEL, max_tokens: 64, messages: [{ role: "user", content: "[[truncate]]" }],
  }).finalMessage();
  fail("truncated stream yields no final message", JSON.stringify(m.stop_reason));
} catch (e) {
  check("truncated stream yields no final message", e instanceof Anthropic.APIError);
}

// ── 7. unknown model ──────────────────────────────────────────────────────────
try {
  await client.messages.create({
    model: "does-not-exist", max_tokens: 8, messages: [{ role: "user", content: "x" }],
  });
  fail("unknown model raises 404", "no error raised");
} catch (e) {
  check("unknown model raises 404", e.status === 404);
}

if (failed) process.exit(1);
console.log("ALL PASS (anthropic node)");
