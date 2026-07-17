//go:build sqlite_vec

package cmd

// precheckTestMainPath is a backend-appropriate mainPath for the generic
// precheckVectorFeatures tests. On a sqlite_vec build (with or without
// pgvector) a SQLite filesystem path is a supported backend, so the precheck
// does not fail fast on missing build-tag support and the cron/validate paths
// are exercised as intended.
const precheckTestMainPath = "/tmp/msgvault.db"
