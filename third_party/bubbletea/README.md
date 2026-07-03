# Patched bubbletea v1.3.10

Verbatim copy of `github.com/charmbracelet/bubbletea@v1.3.10` (root package
only, tests dropped) with **one** change: `tea_init.go` is deleted.

## Why

Upstream `tea_init.go` runs `lipgloss.HasDarkBackground()` from a package
`init()`, which sends an OSC-11 background-color query to the terminal.
Because the msgvault `tui` command links bubbletea into the single binary,
that init ran for *every* command — `msgvault sync`, `--help`, even unknown
commands — probing the terminal before dispatch. Interactively this races
with shell-prompt queries and can leak `^[]11;rgb:...` responses into the
session; on consoles that never answer the query it can stall startup
(see roborev bf1eeb63 for the same failure class in lipgloss v2's compat
package).

The init exists to warm lipgloss's background-color cache *before* a tea
Program takes over the terminal (otherwise the first adaptive-color render
inside a running Program can eat the query response and hang for the OSC
timeout). msgvault keeps that protection, scoped to where it is needed:
`cmd/msgvault/cmd/tui.go` performs the same warm-up call immediately before
`tea.NewProgram(...).Run()`.

`cmd/msgvault/cmd/tui_probe_test.go` guards the regression: it fails if the
resolved bubbletea module ever contains `tea_init.go` again (e.g. if the
`replace` directive in go.mod is dropped).

## Updating

bubbletea v1 is in maintenance (v2 removed this init entirely), so updates
should be rare. To update: copy the new version's root `*.go` files, go.mod,
go.sum, and LICENSE here; delete `tea_init.go` and `*_test.go`; run
`go mod tidy && make test`. The long-term fix is migrating the TUI to
bubbletea v2, which drops the import-time probe upstream.
