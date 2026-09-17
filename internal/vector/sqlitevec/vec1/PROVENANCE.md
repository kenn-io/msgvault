---
last_edited: 2026-09-15
---

# SQLite Vec1 provenance

- Project: SQLite Vec1
- Version: 0.7
- Upstream check-in: `1965bb9e53c83a85c36fe9f911c832183a11cf50845c1dbf9f2270d5a2d38668`
- Source: `https://sqlite.org/vec1/raw/vec1.c?ci=1965bb9e53c83a85c36fe9f911c832183a11cf50845c1dbf9f2270d5a2d38668`
- Retrieved: 2026-09-15
- `vec1.c` SHA-256: `b4bc039d0b5ecb5d7749f95b9fdc2a8224228aacf59518bb3e37afa6535d7662`

The upstream source dedicates itself to the public domain. It is vendored
unchanged so tagged msgvault builds do not require a separately installed Vec1
shared library.

`sqlite3.h` is copied unchanged from `github.com/mattn/go-sqlite3` v1.14.50,
the version pinned by this module. It declares SQLite 3.53.4 and has SHA-256
`4e7d1523cf95991f7e4c08c576e2232e063da6f10067e5c03c9bf9f904b1cf5f`.
Keeping the matching header beside Vec1 lets cgo compile against the SQLite
amalgamation already linked by go-sqlite3 instead of introducing a second
system SQLite library.
