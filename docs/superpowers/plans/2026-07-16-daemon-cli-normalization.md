# Daemon CLI Normalization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `msgvault daemon start|status|stop|restart` the canonical lifecycle interface, retain hidden `serve` compatibility aliases, and make documentation consistently recommend the daemon commands while preserving the existing port-selection contract.

**Architecture:** Construct two independent Cobra subcommand sets from shared factories because one command instance cannot have two parents. Both sets call the existing lifecycle functions; the `daemon` set is visible and the `serve` set is hidden. Runtime lifecycle and port-selection behavior remain unchanged except for user-facing remedy text.

**Tech Stack:** Go, Cobra, testify, Kit daemon runtime records, Markdown/Zensical documentation.

## Global Constraints

- `msgvault serve` remains the visible foreground server command.
- `msgvault daemon start|status|stop|restart` is the documented lifecycle path.
- `msgvault serve start|status|stop|restart` remains accepted without warnings but is hidden from help and completion.
- All Go tests use testify and run with `-tags "fts5 sqlite_vec"`.
- Do not add tautological tests that inspect function pointers or source text.
- Local/default port selection remains `api_port = 0`; explicit nonzero ports are honored exactly.
- Remote, NAS, container, direct-HTTP, and port-forwarding examples retain stable explicit ports.
- Remove `docs/superpowers/specs/2026-07-16-daemon-cli-normalization-design.md` and this plan before final handoff.

---

### Task 1: Canonical and compatibility command surfaces

**Files:**
- Modify: `cmd/msgvault/cmd/serve.go`
- Modify: `cmd/msgvault/cmd/serve_lifecycle.go`
- Modify: `cmd/msgvault/cmd/serve_lifecycle_test.go`

**Interfaces:**
- Consumes: existing `runServeStart`, `runServeStatusWithAPIKey`, `runServeStopWithAPIKey`, and `runServeRestart` lifecycle functions.
- Produces: `daemonCmd`, `newDaemonCommand()`, and fresh visible/hidden lifecycle command instances.

- [ ] **Step 1: Write failing command-surface tests**

Replace `TestServeCommandHasLifecycleSubcommands` and add behavioral coverage in `serve_lifecycle_test.go`:

```go
func TestDaemonAndServeLifecycleCommandSurfaces(t *testing.T) {
	assert := assert.New(t)

	daemonNames := map[string]bool{}
	for _, sub := range daemonCmd.Commands() {
		daemonNames[sub.Name()] = true
		assert.False(sub.Hidden, "daemon %s must be visible", sub.Name())
	}
	for _, name := range []string{"start", "status", "stop", "restart"} {
		assert.True(daemonNames[name], "daemon must expose %s", name)
		compat, _, err := serveCmd.Find([]string{name})
		require.NoError(t, err)
		assert.Equal(name, compat.Name())
		assert.True(compat.Hidden, "serve %s must be hidden", name)
	}
}

func TestDaemonAndServeStatusHaveIdenticalBehavior(t *testing.T) {
	dataDir := t.TempDir()
	oldCfg := cfg
	cfg = lifecycleTestConfig(dataDir)
	t.Cleanup(func() { cfg = oldCfg })

	run := func(args ...string) (string, error) {
		root := newTestRootCmd()
		root.SilenceUsage = true
		root.AddCommand(newDaemonCommand())
		compatServe := &cobra.Command{Use: "serve"}
		addServeLifecycleCommands(compatServe)
		root.AddCommand(compatServe)
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		return stdout.String(), root.ExecuteContext(context.Background())
	}

	daemonOut, daemonErr := run("daemon", "status")
	serveOut, serveErr := run("serve", "status")
	require.NoError(t, daemonErr)
	require.NoError(t, serveErr)
	assert.Equal(t, daemonOut, serveOut)
	assert.Equal(t, "No msgvault daemon is running.\n", daemonOut)
}
```

Add `io` to imports.

- [ ] **Step 2: Run the tests and confirm the intended failure**

Run:
```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestDaemonAndServe(LifecycleCommandSurfaces|StatusHaveIdenticalBehavior)' -count=1
```
Expected: FAIL because the daemon factories do not exist and the serve children are visible.

- [ ] **Step 3: Implement shared command factories**

Replace the four literal command definitions in `serve_lifecycle.go` with this shape:

```go
func newLifecycleCommand(name string, hidden bool) *cobra.Command {
	cmd := &cobra.Command{Use: name, Hidden: hidden, Args: cobra.NoArgs}
	switch name {
	case "start":
		cmd.Short = "Start msgvault daemon in the background"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error { return runServeStart(cmd, cfg) }
	case "status":
		cmd.Short = "Show msgvault daemon status"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			return runServeStatusWithAPIKey(cmd, cfg.Data.DataDir, cfg.Server.APIKey)
		}
	case "stop":
		cmd.Short = "Stop msgvault daemon"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			return runServeStopWithAPIKey(cmd, cfg.Data.DataDir, cfg.Server.APIKey)
		}
	case "restart":
		cmd.Short = "Restart msgvault daemon in the background"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error { return runServeRestart(cmd, cfg) }
	default:
		panic("unknown daemon lifecycle command: " + name)
	}
	return cmd
}

func addServeLifecycleCommands(parent *cobra.Command) {
	for _, name := range []string{"start", "status", "stop", "restart"} {
		parent.AddCommand(newLifecycleCommand(name, true))
	}
}

func newDaemonCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "daemon", Short: "Manage the background daemon", Args: cobra.NoArgs}
	for _, name := range []string{"start", "status", "stop", "restart"} {
		cmd.AddCommand(newLifecycleCommand(name, false))
	}
	return cmd
}

var daemonCmd = newDaemonCommand()
```

In `serve.go`, register `daemonCmd` on `rootCmd` and call
`addServeLifecycleCommands(serveCmd)`. Do not hide bare `serve`. Update the one
existing test that calls `serveStatusCmd.RunE` directly to locate the hidden
status child with `serveCmd.Find([]string{"status"})` and execute that production
command instead.

- [ ] **Step 4: Run targeted tests and commit**

Run:
```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestDaemonAndServe|TestRunServeStatus' -count=1
```
Expected: PASS.

Commit the three task files using the mandatory commit skill with a rationale-first message.

---

### Task 2: Explicit-port contract coverage

**Files:**
- Modify: `cmd/msgvault/cmd/serve_test.go`

**Interfaces:**
- Consumes: `listenServeAPI` and `listenerPort`.
- Produces: behavioral coverage proving an available explicit port is honored.

- [ ] **Step 1: Verify existing coverage**

Run:
```bash
go test -tags "fts5 sqlite_vec" ./internal/config -run 'TestServerConfigDefaults|TestSaveAndLoad_RoundTrip' -count=1
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'TestRunServeAutoSelectsAPIPortWhenUnconfigured|TestRunServeFailsBeforeArchiveWorkWhenAPIPortInUse' -count=1
```
Expected: PASS. Do not duplicate default, round-trip, auto-selection, or occupied-port tests.

- [ ] **Step 2: Add the missing characterization test**

Add to `serve_test.go`:

```go
func TestListenServeAPIHonorsAvailableExplicitPort(t *testing.T) {
	probe, err := net.Listen("tcp", net.JoinHostPort(defaultDaemonBindAddr, "0"))
	require.NoError(t, err)
	explicitPort, err := listenerPort(probe)
	require.NoError(t, err)
	require.NoError(t, probe.Close())

	ln, err := listenServeAPI(defaultDaemonBindAddr, explicitPort)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	actualPort, err := listenerPort(ln)
	require.NoError(t, err)
	assert.Equal(t, explicitPort, actualPort)
}
```

This covers existing behavior and should pass immediately; no production port change is planned.

- [ ] **Step 3: Run port tests and commit**

Run:
```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'Test(ListenServeAPIHonorsAvailableExplicitPort|RunServeAutoSelectsAPIPortWhenUnconfigured|RunServeFailsBeforeArchiveWorkWhenAPIPortInUse)' -count=1
```
Expected: PASS. Commit `serve_test.go` using the mandatory commit skill.

---

### Task 3: Canonical lifecycle guidance

**Files:**
- Modify: `cmd/msgvault/cmd/daemon_runtime_test.go`
- Modify: `cmd/msgvault/cmd/direct_write.go`
- Modify: `cmd/msgvault/cmd/direct_write_test.go`
- Modify: `cmd/msgvault/cmd/serve_lifecycle.go`
- Modify: `cmd/msgvault/cmd/serve_lifecycle_test.go`
- Modify: `cmd/msgvault/cmd/serve_ownership.go`
- Modify: `cmd/msgvault/cmd/serve_ownership_test.go`
- Modify: `cmd/msgvault/cmd/store_resolver.go`
- Modify: `cmd/msgvault/cmd/store_resolver_test.go`
- Modify: `cmd/msgvault/cmd/unpack_attachments.go`
- Modify: `cmd/msgvault/cmd/unpack_attachments_test.go`

**Interfaces:**
- Consumes: existing lifecycle errors and help strings.
- Produces: operator remedies that point to `msgvault daemon start|status|stop|restart`.

- [ ] **Step 1: Change assertions first**

Update exact/substr assertions from `serve stop`, `serve status`, or `serve restart` to `msgvault daemon ...`. Add or update launch-contention coverage to require:

```text
msgvault daemon start is already in progress.
```

Do not alter fixture text such as `--- serve start ---` when it represents a log marker.

- [ ] **Step 2: Run and confirm failures**

Run:
```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd -run 'Test(ArchiveOwned|Direct|Unpack|ClaimServeOwnership|RunServeStart|Daemon|OpenHTTPStore|LocalDaemon|WaitForUsable)' -count=1
```
Expected: FAIL because production guidance still uses `serve`.

- [ ] **Step 3: Update production guidance**

Make these replacements:

- `direct_write.go`: archive ownership remedies use `msgvault daemon stop`.
- `unpack_attachments.go`: long help and refusal use `msgvault daemon stop`.
- `serve_ownership.go`: ownership failure uses `msgvault daemon stop` and `msgvault daemon status`.
- `serve_lifecycle.go`: compatibility remedy uses `msgvault daemon stop`; contention output uses `msgvault daemon start`.
- `store_resolver.go`: compatibility and authentication remedies use `msgvault daemon restart` and `msgvault daemon stop`.

Update adjacent user-facing comments without renaming internal identifiers.

- [ ] **Step 4: Prove the sweep and commit**

Run:
```bash
rg -n 'msgvault serve (start|status|stop|restart)|serve (start|status|stop|restart)' cmd/msgvault/cmd --glob '*.go'
```
Expected remaining matches: internal comments or non-guidance fixtures only, after manual inspection.

Re-run the targeted test command from Step 2 and expect PASS. Commit all task files using the mandatory commit skill.

---

### Task 4: Documentation, cleanup, verification, and PR

**Files:**
- Modify: `README.md`
- Modify: `docs/api-server.md`
- Modify: `docs/cli-reference.md`
- Modify: `docs/configuration.md`
- Modify: `docs/guides/daemon-migration.md`
- Modify: `docs/internal/packed-attachments-design.md`
- Delete: `docs/superpowers/specs/2026-07-16-daemon-cli-normalization-design.md`
- Delete: `docs/superpowers/plans/2026-07-16-daemon-cli-normalization.md`

**Interfaces:**
- Consumes: final CLI surface and current configuration reference.
- Produces: docs that recommend `daemon` and distinguish automatic local ports from stable deployment/API ports.

- [ ] **Step 1: Update docs with explicit classification**

- `README.md`: recommend `daemon` in lifecycle/unpack examples; retain bare `serve`; retain its `8080`/`0.0.0.0`/API-key remote block.
- `docs/cli-reference.md`: add visible `daemon` section; make `serve` foreground-first; include one note that hidden `serve` lifecycle aliases remain accepted; use `daemon start` for idle timeout.
- `docs/guides/daemon-migration.md`: change operational examples/remedies to `daemon`; retain bare `serve` for foreground.
- `docs/configuration.md`: use `daemon start` in lifecycle wording and change the generic loopback example to `api_port = 0`.
- `docs/api-server.md`: use `daemon start` in lifecycle wording; retain `8080` because direct curl access requires a stable port.
- `docs/internal/packed-attachments-design.md`: recommend `msgvault daemon stop`.
- Leave historical `docs/changelog.md` lifecycle wording unchanged.
- Leave `docs/guides/remote-deployment.md` at `8080` for container mapping.

- [ ] **Step 2: Verify documentation terminology**

Run:
```bash
rg -n 'msgvault serve (start|status|stop|restart)|serve (start|status|stop|restart)' README.md docs --glob '*.md' --glob '!changelog.md' --glob '!superpowers/**'
rg -n 'api_port = (0|8080)|auto-select|stable port' README.md docs --glob '*.md' --glob '!superpowers/**'
```
Expected: lifecycle matches outside the changelog are limited to the compatibility note; local/generic config uses `0`, while remote/direct-HTTP/container examples use stable ports.

- [ ] **Step 3: Delete temporary Superpowers artifacts**

Delete the design spec and this plan with `apply_patch`. Remove empty `docs/superpowers` directories if no other files remain.

- [ ] **Step 4: Run full isolated verification**

Do not run `make install` or use live `~/.msgvault` state. Run:
```bash
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
make test
make lint-ci
make build
```
Expected: all exit 0.

- [ ] **Step 5: Drive the built CLI against scratch state**

Run the repository build artifact with a temporary `MSGVAULT_HOME`:
```bash
scratch=$(mktemp -d)
MSGVAULT_HOME="$scratch" ./msgvault daemon status
MSGVAULT_HOME="$scratch" ./msgvault serve status
MSGVAULT_HOME="$scratch" ./msgvault daemon --help
MSGVAULT_HOME="$scratch" ./msgvault serve --help
```
Observe identical stopped-state status, four visible daemon lifecycle commands, and no lifecycle entries in serve help. Remove the scratch directory.

- [ ] **Step 6: Scrub and commit final docs/cleanup**

Run the private-data scrub over the complete public diff and unpushed commits, including commit messages. Require zero hits. Commit documentation and Superpowers-doc removal using the mandatory commit skill. Verify `git status --short` is empty and no `docs/superpowers` files remain.

- [ ] **Step 7: Push and open the PR**

Review the full branch diff against its base, push the current branch to `origin`, and create a rationale-first PR. Explain that a canonical lifecycle interface reduces operator friction while hidden aliases preserve automation, and that automatic local port selection remains distinct from stable remote/direct-HTTP port configuration. Do not include a mechanical validation transcript.
