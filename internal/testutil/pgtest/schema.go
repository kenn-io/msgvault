// Package pgtest coordinates PostgreSQL test fixtures without importing store.
package pgtest

import "sync"

// SchemaDDL serializes whole-schema initialization and cleanup across test
// helpers. Concurrent operations can exhaust PostgreSQL's shared lock table.
// ponytail: per-process lock; use a server advisory lock if test processes need coordination.
var SchemaDDL sync.Mutex
