# Daemon CLI Normalization Design

## Goal

Make `msgvault daemon start|status|stop|restart` the canonical, documented
interface for managing the local background daemon while preserving the
existing `msgvault serve` lifecycle subcommands as silent compatibility
aliases.

The foreground web server remains `msgvault serve`.

## Command Surface

The root command gains a visible `daemon` command with four visible
subcommands:

```text
msgvault daemon start
msgvault daemon status
msgvault daemon stop
msgvault daemon restart
```

Each command uses the existing Kit-backed lifecycle implementation and retains
its current output, idempotence, shutdown behavior, version compatibility
checks, and configuration loading.

The existing lifecycle forms remain accepted:

```text
msgvault serve start
msgvault serve status
msgvault serve stop
msgvault serve restart
```

These compatibility subcommands are hidden from `msgvault serve --help` and
are not presented as an alternate recommended workflow. They invoke the same
handlers as the canonical commands so the two surfaces cannot drift.

`msgvault serve` itself remains visible and continues to run the server in the
foreground until interrupted.

## Implementation Boundaries

Command construction will be separated from lifecycle behavior. Small command
factory functions will create the canonical daemon subcommands and the hidden
serve compatibility subcommands, with both sets delegating to the existing
`runServeStart`, `runServeStatusWithAPIKey`, `runServeStopWithAPIKey`, and
`runServeRestart` functions.

This change does not redesign daemon process management, runtime records,
locking, shutdown, health reporting, auto-restart policy, or remote-daemon
routing. The existing lifecycle code remains authoritative.

## Port Selection

Local daemon configuration continues to use `api_port = 0` as the default.
Omitting `api_port` has the same result. In either case the listener asks the
operating system for an available port, and the daemon publishes the actual
bound port in its runtime record for local client discovery.

Any nonzero `api_port` is an explicit operator choice and must be honored
exactly. Startup fails with the existing bind error when that port is not
available; msgvault must not silently fall back to another port.

Generic and local configuration examples will show `api_port = 0` or omit the
field. Remote, NAS, container, and port-forwarding examples will retain an
explicit stable port such as `8080`, because those deployments require a
predictable external mapping.

## Documentation

README examples and current user, configuration, migration, troubleshooting,
and CLI-reference documentation will recommend `msgvault daemon ...` for
background lifecycle management. Operational hints emitted by the CLI will
also point to the canonical `daemon` commands.

The implementation plan will classify each port example before changing it:
generic and local-daemon examples use automatic selection, while remote, NAS,
container, and port-forwarding examples keep their stable explicit ports.

Documentation will distinguish the two visible roles:

- `msgvault serve` runs a foreground server.
- `msgvault daemon start|status|stop|restart` manages the local background
  daemon.

The hidden `serve` lifecycle aliases may be mentioned only in a compatibility
note where migration context requires it. Historical changelog entries remain
unchanged when they describe the command surface shipped by an earlier
release.

## Error Handling and Compatibility

Argument validation remains `cobra.NoArgs` for all lifecycle subcommands.
Canonical and compatibility commands preserve the existing errors and exit
status. The hidden aliases do not emit deprecation warnings, ensuring existing
scripts remain quiet.

Existing guidance embedded in errors and tests will be updated from
`msgvault serve stop` or `msgvault serve restart` to the corresponding
canonical `msgvault daemon` command. The sweep includes archive ownership,
attachment unpacking, daemon ownership, local daemon startup and authentication,
version-compatibility remedies, command help, and their tests. Shared launch
contention output becomes `msgvault daemon start is already in progress`, even
when reached through the hidden compatibility alias.

## Testing

Tests will exercise production command construction and behavior:

- root help exposes `daemon`, and `daemon` exposes all four lifecycle
  subcommands;
- `serve` retains all four lifecycle subcommands, but marks them hidden;
- canonical `daemon status` and compatibility `serve status` are both executed
  through Cobra against an isolated temporary data directory and produce the
  same output and exit behavior;
- existing port-selection tests are reviewed before adding coverage so the
  implementation does not duplicate tests for default configuration, configured
  save/load, automatic selection, or occupied-port failure;
- the missing listener case proves that an available, explicitly configured
  nonzero port is bound exactly rather than replaced with another port.

All Go tests use testify and run with the repository's required build tags.

## Non-Goals

- Removing the `serve` lifecycle compatibility aliases.
- Adding lifecycle flags or command-specific configuration overrides.
- Changing foreground-server semantics.
- Changing remote deployment ports or generated container port mappings.
- Porting unrelated daemon lifecycle or process-safety behavior.
