# Contributing to Mainspring

Thanks for considering it. This project has a deliberately strict workflow — it is what keeps a
single-maintainer codebase reviewable. Everything below is enforced, not aspirational.

## The bar

Mainspring is infrastructure people run on their own hardware. The standard is high:

- **`go test -race ./...` must pass.** If the race detector complains, it ships nothing.
- **`go build ./...` and `go vet ./...` must be clean** before you ask for review, not after.
- **No duplicate logic.** Three copies of the same threshold table is a bug.
- **Dead code is a lie.** A branch that can never execute states something false about the
  program. Delete it.
- **User-visible output must be correct.** Truncating a number to zero is broken, not "close
  enough".
- **Never write an inference kernel.** Backends wrap existing MIT/Apache engines as subprocesses
  or adopted daemons. That is a permanent boundary, not a current limitation.
- **Fail loud, never silently degrade.** If the effective context shrank, or the run fell back to
  CPU, that must surface in `/capabilities` and a header. Silence is the bug this project exists
  to fix.

Smaller is better. If a change is 200 lines and could be 50, make it 50. No speculative features,
no abstractions for a single call site, no error handling for impossible states.

## Workflow — issue first, always

**No code without an issue. No branch without an issue number.**

1. **Open an issue** describing the problem before writing code. For a bug, include steps to
   reproduce, expected behaviour and actual behaviour.
2. **Branch from `develop`** — never from `main`:

   | Type | Pattern |
   |---|---|
   | Feature | `feature/#{issue}-short-desc` |
   | Bug fix | `fix/#{issue}-short-desc` |
   | Chore / deps | `chore/#{issue}-short-desc` |

   The issue number in the branch name is what links the branch to the issue.
3. **Commit with [Conventional Commits](https://www.conventionalcommits.org/).** This drives
   changelog generation and version bumps, so it is not optional.

   ```
   feat(scheduler): evict by byte budget before admitting a load (#42)
   fix(server): flush SSE frames immediately on stream (#43)
   chore(deps): bump golang.org/x/sys (#44)
   ```

   `feat:` → minor · `fix:` / `perf:` → patch · `feat!:` or `BREAKING CHANGE:` → major ·
   `refactor:` / `chore:` / `docs:` / `test:` / `ci:` → no release.
4. **Open a PR against `develop`.** The title must itself be a valid conventional commit, and the
   body must contain `Closes #<issue>` so the issue closes on merge.
5. **Squash merge.** Never force-push `develop` or `main`.

Never open a PR from a feature branch directly to `main`. Releases reach `main` through a release
branch; version numbers are managed by release-please and are never edited by hand.

## Update the docs in the same change

This is a real requirement, not a courtesy. If your change adds or alters a **CLI command, HTTP
endpoint, response header, metric name, or config key**, update these in the *same* PR:

- `README.md` — the feature list
- `docs/llms.txt` — the machine-readable description AI tools read
- `docs/index.html` — the landing page, if the change is user-facing

Documentation must describe only what has actually shipped. Never document a feature that has not
merged.

## Tests

New behaviour needs a test that would fail without your change. A test that only asserts the code
compiles is not a test. When you fix a bug, add the test that would have caught it — several past
bugs in this repo survived precisely because a code path had no coverage at all.

There are three layers, and it is worth knowing which one your change needs.

```bash
go test -race ./...          # everything; the bar for any PR
./test/realengine/run.sh     # opt-in, needs a live engine (see below)
```

**Conformance** (`test/conformance/`) drives the real `openai` and `anthropic` Python and JS SDKs
against a running server, over the happy path and the failure paths. CI runs it on every PR. The
upstream is `test/conformance/fakeserver`, so what it verifies is *our* side of the wire.

**Real engine** (`test/realengine/`) is the same idea pointed at an actual local engine, and it is
the only thing that checks the assumptions the fakeserver encodes rather than Mainspring's side of
them. It needs a daemon and real weights, so it is `workflow_dispatch` only — never on the PR path.

```bash
ollama serve &
ollama pull qwen2.5:0.5b
./test/realengine/run.sh

REAL_MODEL=llama3.2:1b ./test/realengine/run.sh   # any small chat model works
PYTHON=./venv/bin/python ./test/realengine/run.sh  # a virtualenv, unactivated
```

Every assertion there is about mechanism, not output quality, so the model choice does not matter —
that is deliberate, because a suite whose result depends on whether a 0.5B model followed an
instruction is a suite nobody trusts. Run it before a release, and after touching anything that
assumes how an engine behaves: stop handling, usage accounting, the adopt backends. It refuses loudly
and exits 2 when it cannot run; it never skips quietly, because a silent skip reads as a pass.

It earns its keep: its first run found the silently-ignored config key (#197) and the silently-dropped
`n` (#198), and confirmed the upstream behaviour #192's design depends on.

## Reporting security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).
