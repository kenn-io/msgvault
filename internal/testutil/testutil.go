// Package testutil provides test helpers for msgvault tests.
//
// The package is organized into focused files:
//   - assert.go: small generic helpers (MakeSet)
//   - store_helpers.go: database test setup (NewTestStore)
//   - fs_helpers.go: filesystem operations (WriteFile, ReadFile, MustExist)
//   - archive_helpers.go: archive creation (CreateTarGz, CreateTempZip)
//   - security_data.go: security test vectors (PathTraversalCases)
//   - builders.go: test data builders
//   - encoding.go: encoding test helpers
package testutil
