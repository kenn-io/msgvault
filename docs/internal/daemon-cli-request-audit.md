# Daemon CLI request audit

| Interaction family | Production construction | HTTP form | Cancellation source | Duration policy |
| --- | --- | --- | --- | --- |
| Cobra archive commands | `OpenHTTPStore(cmd.Context())` | generated JSON | explicit method context or client root context | authenticated CLI |
| TUI | `OpenHTTPStore(ctx)` | generated JSON and raw downloads | TUI command context | authenticated CLI |
| MCP | `OpenHTTPStore(cmd.Context())` | generated JSON, search, attachments, manifests | MCP command context | authenticated CLI |
| Streaming maintenance | `OpenHTTPStore(cmd.Context())` | NDJSON streaming | method context; gate waits use the same root | authenticated CLI |
| Raw SQL and aggregates | `OpenHTTPStore(cmd.Context())` | generated JSON | method context through handler to DuckDB `QueryContext` | authenticated CLI; DuckDB cancellation integration-tested |
| Legacy store adapters | client returned by `OpenHTTPStore` | generated JSON | stored root command context | authenticated CLI |
| Backup freeze begin/end | `newDaemonCLIClient(cmd.Context(), ...)` | generated JSON | backup command context | authenticated CLI |
| Local authentication probe | direct bounded `http.Request` | authenticated health JSON | two-second probe context | bounded liveness |
| Browser and third-party API | outside internal CLI client | Huma/API | request context | existing bounded path policy |

Central invariants:

- The request editor marks generated, raw, and streaming calls.
- Timeout eligibility uses `requestAuthentication`.
- CLI mode clones but does not replace the transport.
- Authenticated CLI and legacy long paths share read/write deadline clearing.
- Unmarked raw SQL retains `QueryEndpointTimeout`.
- All three production daemon-client creation paths converge on one helper.
