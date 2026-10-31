package cmd

const rootHelp = `Archive email, chats, and meetings offline. Commands use the local daemon
(started on demand) or config.toml's [remote].url; --local forces local.

Start here: agent workflow
  msgvault setup status --json              # 1. inspect setup and missing capabilities
  msgvault account list --json              # 2. find accounts and source IDs
  msgvault source gmail add you@example.com # 3. connect a source; see source --help
  msgvault sync full you@example.com        # 4. first mail sync; later: msgvault sync
  msgvault search 'from:alice@example.com newer_than:30d' --json
  msgvault show <id> --json                 # 5. read a search result
  msgvault thread show <id> --json          #    read its conversation
  msgvault export eml <id> -o msg.eml        # 6. save a message
  msgvault export messages --start 2026-09-01T00:00:00Z --end 2026-10-01T00:00:00Z
  msgvault person --help
  msgvault daemon status

IDs and targets:
  Message IDs come from search's id field. show also accepts provider message
  IDs. Person, organization, source and generation IDs come from their list
  commands; deletion batch IDs come from deletion list. Collections use names.
  Accounts accept an identifier (email or source name); --source-id selects an
  exact source where supported. Use account list to resolve ambiguous names.
  --home overrides MSGVAULT_HOME (default ~/.msgvault); --config selects a TOML
  file. Agent-delegated mode uses --agent-url and --agent-token-file; restricted
  grants are issued by the owner with agent-token issue. See agent-token --help.

Output and errors:
  Human text is the default. Data commands offer --json where listed in their
  help; export messages writes JSON Lines. Success data stays on stdout.
  Diagnostics, progress and errors use stderr. Errors end with hint: <next step>.
  With --json, the final error is a JSON line on stderr:
  {"error":{"code":"usage","message":"...","hint":"...","exit_code":2}}
  Existing warnings/progress may precede it. --log-level=warn quiets routine logs.

Exit codes:
  0 success; 1 internal/unclassified; 2 usage (command/flags/argument count);
  3 invalid value; 4 not found; 5 not initialized; 6 conflict; 7 unavailable/auth;
  75 sync paused; 130 interrupted. A daemon may report paused as a sync outcome.

Environment:
  MSGVAULT_HOME selects the local home. MSGVAULT_IMAP_PASSWORD supplies an IMAP
  password. MSGVAULT_REMOTE_URL and MSGVAULT_REMOTE_API_KEY are used by account
  export-token. VOYAGE_API_KEY / OPENAI_API_KEY supply configured embeddings.
  MSGVAULT_ENABLE_REMOTE_DELETE is owner opt-in for remote deletion.
  Check a command's --help for its provider-specific setup and flags.

More workflows and examples: msgvault quickstart.`
