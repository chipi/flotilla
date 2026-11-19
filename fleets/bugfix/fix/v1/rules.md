# Rules for this run (flotilla's bugfix fleet)

You run headless, without a person. Every tool result stays in this conversation and is sent
again with each later step, so keep what you read and run small.

- Find before you read: `grep -rn` (or the grep tool) for the names in the bug report, then read
  the lines around the matches. For a file over 300 lines, read a range (offset and limit), not
  the whole file.
- Do not read generated, built or vendored files: `node_modules/`, `dist/`, `build/`,
  `.svelte-kit/`, `__pycache__/`, `.venv/`, compiled or bundled output (such as compiled message
  catalogs), `*.min.js`, lockfiles. Read their sources instead.
- Do not dump history: no `git log -p`, no `git log --all`, no `git show` of a whole commit.
  `git log --oneline -n 20 -- <path>` and `git show <sha> -- <path>` are enough.
- Keep command output short: run only the test files that matter, not the whole suite, and keep
  the end of long output, where test failures are (`2>&1 | tail -n 60`).
- Do not commit, push, open a PR, or change CI, hooks, config or the lockfile.
