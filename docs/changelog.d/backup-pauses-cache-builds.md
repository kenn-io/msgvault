### Bug fixes

- `msgvault backup create` no longer fails with "wal_checkpoint stayed busy" while the daemon is building the analytics cache. The daemon stops its cache builds for the freeze and restarts them afterward. If a cache build in another process keeps the archive open, the backup fails within 30 seconds and the error names it.
