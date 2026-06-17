# AGENTS.md

This file applies to all AI coding agents (Claude Code, Codex, Copilot CLI, etc.) operating in this repo.

## Authoritative References

- Project guide: `CLAUDE.md` (this file is authoritative; AGENTS.md inherits its rules).

## Testing — Use testify

All Go tests use `github.com/stretchr/testify`. New tests and modifications to existing tests MUST use `assert.X` or `require.X` from testify — never `t.Errorf`, `t.Fatalf`, `t.Fatal`, or `t.Error`.

- `require.X` halts on failure (was `t.Fatal*`). Use for setup or fatal preconditions.
- `assert.X` continues on failure (was `t.Error*`). Use for independent value checks.
- Equality takes `(want, got)`, not `(got, want)`. Always: `assert.Equal(t, want, got)`.

The mapping cheatsheet is in `CLAUDE.md` under the Testing section.

## Custom Helpers

`internal/testutil` retains non-assertion helpers (`MakeSet`, `NewTestStore`, fixture builders, etc.). It no longer provides `AssertEqual` / `MustNoErr` / similar — those were removed in favor of calling testify directly.

## Build Tags

All `go test` invocations need `-tags "fts5 sqlite_vec"`. Prefer `make test` to get the tags automatically.

## Commits

Every code-producing turn ends with a commit. See `CLAUDE.md` for details. Do not ask for permission to commit.
