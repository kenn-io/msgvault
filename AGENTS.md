# AGENTS.md

This file applies to all AI coding agents (Claude Code, Codex, Copilot CLI, etc.) operating in this repo.

## Authoritative References

- Project guide: `CLAUDE.md` (this file is authoritative; AGENTS.md inherits its rules).

## Roborev

- Never manually invoke `roborev review` in any form unless the user explicitly
  asks for it. Never invoke a roborev skill (including `roborev-fix` or
  `roborev-design-review-branch`) unless the user explicitly asks for that
  skill.

## Testing — Use testify

All Go tests use `github.com/stretchr/testify`. New tests and modifications to existing tests MUST use `assert.X` or `require.X` from testify — never `t.Errorf`, `t.Fatalf`, `t.Fatal`, or `t.Error`.

- `require.X` halts on failure (was `t.Fatal*`). Use for setup or fatal preconditions.
- `assert.X` continues on failure (was `t.Error*`). Use for independent value checks.
- Equality takes `(want, got)`, not `(got, want)`. Always: `assert.Equal(t, want, got)`.

The mapping cheatsheet is in `CLAUDE.md` under the Testing section.

## Testing — No Fake TDD

Do not add tautological tests that copy shell scripts into synthetic temp trees,
stub the primary commands, and only assert that the stub saw expected arguments.
Exercise the production path, a real validator/parser, or a built artifact
instead. See `CLAUDE.md` for the full rule and narrow exception.

Do not add bash tests that grep shell scripts, workflows, config files, or docs
for expected implementation text. Those checks are usually tautological; prefer
real execution, parser/tool-native validation, or a documented manual release
check.

## Custom Helpers

`internal/testutil` retains non-assertion helpers (`MakeSet`, `NewTestStore`, fixture builders, etc.). It no longer provides `AssertEqual` / `MustNoErr` / similar — those were removed in favor of calling testify directly.

## Build Tags

All `go test` invocations need `-tags "fts5 sqlite_vec"`. Prefer `make test` to get the tags automatically.

## Commits

Every code-producing turn ends with a commit. See `CLAUDE.md` for details. Do not ask for permission to commit.
