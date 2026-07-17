//go:build pgvector && !sqlite_vec

package cmd

// precheckTestMainPath is a backend-appropriate mainPath for the generic
// precheckVectorFeatures tests. On a pgvector-only build (no sqlite_vec) a
// SQLite path would trip the "built without sqlite-vec support" fast-fail
// before the cron/validate checks run, so the generic tests use a postgres://
// DSN, whose backend (pgvector) IS available under this tag combo.
const precheckTestMainPath = "postgres://user@localhost:5432/msgvault"
