### Bug fixes

- Update DuckDB to v2.0.1 (`duckdb-go` v2.20001.0) so filtered `v_messages` queries on large archives no longer fail with an internal `dict-surviving join` error when an intermediate join spills ([msgvault#2337](https://github.com/kenn-io/msgvault/issues/2337)).
