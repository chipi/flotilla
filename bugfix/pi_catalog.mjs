// bugfix/pi_catalog.mjs: what pi's own catalog knows about an OpenRouter model (its context window, output
// limit, prices, reasoning and compat), for bugfix/pi_config.py. Through LiteLLM pi sees a custom provider
// and falls back to a 128k window and 16k output, and no prices; the bake-off ran on pi's openrouter
// provider, which had these. Found 2026-11-15: a fix session stopped at 124k of context with one token.
//   node bugfix/pi_catalog.mjs deepseek/deepseek-v4-flash z-ai/glm-5.2   -> {"deepseek/...": {...}, ...}
import { execSync } from "node:child_process";
import path from "node:path";
import { pathToFileURL } from "node:url";

const root = process.env.PI_CATALOG_ROOT || execSync("npm root -g", { encoding: "utf8" }).trim();
const dir = path.join(root, "@earendil-works", "pi-coding-agent", "node_modules", "@earendil-works", "pi-ai", "dist", "providers");
const { OPENROUTER_MODELS } = await import(pathToFileURL(path.join(dir, "openrouter.models.js")).href);
const out = {};
for (const id of process.argv.slice(2)) {
  const m = OPENROUTER_MODELS[id];
  if (!m) {
    process.stderr.write(`pi's catalog has no openrouter model ${id}\n`);
    process.exit(3);
  }
  // not thinkingFormat or the thinking levels: those shape OpenRouter's own request fields, and LiteLLM is
  // in between
  const { thinkingFormat, ...compat } = m.compat || {};
  out[id] = { reasoning: m.reasoning, input: m.input, cost: m.cost, contextWindow: m.contextWindow, maxTokens: m.maxTokens, compat };
}
process.stdout.write(JSON.stringify(out));
