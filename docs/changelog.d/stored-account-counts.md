### Improvements

- Account lists from `msgvault list-accounts`, MCP, and the API read each account's message counts from totals the archive keeps current on every write, instead of counting the archive's messages on each request. Lists stay fast on large PostgreSQL archives and right after a restart, and counts are never shown as pending or stale. The first upgrade counts each account once.
