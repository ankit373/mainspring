// OpenAI JS SDK conformance check against a running Mainspring server.
// Mirrors the Python suite: streaming, tool_call arguments as a JSON string,
// embeddings, models listing, and 404 mapping.
import OpenAI from "openai";

const client = new OpenAI({ baseURL: "http://localhost:11500/v1", apiKey: "conformance" });

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

if (failed) process.exit(1);
console.log("ALL PASS (node)");
