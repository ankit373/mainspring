# docs/

Static site for Mainspring, served via GitHub Pages (Settings → Pages → deploy from `docs/` on the
default branch).

- `index.html` — landing page (features, backends, quick start, API/CLI). Self-contained, no external
  assets or fonts.
- `llms.txt` — AI-context file (what ChatGPT / Claude / Perplexity read). Keep it factual: it must
  reflect what `mainspring --help` actually does — no aspirational features.

When the CLI or API surface changes, update both files in the same PR.
