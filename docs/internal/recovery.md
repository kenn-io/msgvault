# Database Recovery

`msgvault verify you@gmail.com` runs `PRAGMA integrity_check` against the
SQLite database before the Gmail comparison step. When that check fails,
the recovery path depends on which part of the database is affected.

## Search-index (FTS5) corruption

Symptom (from `msgvault verify` output):

```
malformed inverted index for FTS5 table main.messages_fts
```

Fix:

```
msgvault rebuild-fts
```

This drops `messages_fts` and recreates it from the core tables
(`messages`, `message_bodies`, `message_recipients`, `participants`). Peak
extra disk usage is roughly the size of the FTS5 shadow tables — a few
percent of the SQLite database.

By default, `rebuild-fts` runs through a configured remote or the local
background daemon over HTTP and streams progress back to the CLI. This keeps
the SQLite archive owned by the daemon during the repair.

Pass `--local` only when a remote is configured and you want the rebuild to run
through this machine's local background daemon instead.

### Why SQLite's own rebuild pragma does not work

`INSERT INTO messages_fts(messages_fts) VALUES('rebuild')` regenerates the
FTS5 inverted index from the contentful shadow tables themselves. If those
shadow tables are already malformed, the pragma reads the corruption right
back out.

`INSERT INTO messages_fts(messages_fts) VALUES('delete-all')` is rejected
with `'delete-all' may only be used with a contentless or external content
fts5 table`. msgvault's `messages_fts` is contentful by design (it stores
its own copy of the searchable text), so `delete-all` is not available.

`rebuild-fts` sidesteps both: it drops the virtual table entirely — which
removes the shadow tables — then recreates it fresh and repopulates from
the core tables.

## Core-table corruption

Symptom:

```
Tree 26 page 8231140 cell 2: Rowid 421177 out of order
non-unique entry in index sqlite_autoindex_messages_1
```

Fix (requires free disk roughly equal to the size of the database):

```
sqlite3 ~/.msgvault/msgvault.db '.recover' | sqlite3 ~/.msgvault/recovered.db
mv ~/.msgvault/msgvault.db ~/.msgvault/msgvault.db.bak
mv ~/.msgvault/recovered.db ~/.msgvault/msgvault.db
msgvault verify you@gmail.com
```

A leaner alternative that works on cleaner corruption:

```
sqlite3 ~/.msgvault/msgvault.db .dump | sqlite3 ~/.msgvault/new.db
```

If free disk is tight, individual corrupt rows can sometimes be repaired
by hand — delete and re-insert the affected row(s) from their source
(MIME blob, etc.). This is a last resort and only advisable if you can
identify the specific rows flagged by `integrity_check`.

## Before any repair

Back up the database file. If the repair tool is interrupted or makes
things worse, the backup is the only way back:

```
cp ~/.msgvault/msgvault.db ~/.msgvault/msgvault.db.bak
```

If the database has any activity, also copy the `-wal` and `-shm` sidecar
files at the same instant (or run `msgvault` once to checkpoint the WAL
into the main file before copying). A bare `.db` copy without its sidecars
can itself be a source of corruption.
