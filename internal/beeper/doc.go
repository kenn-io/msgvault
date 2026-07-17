// Package beeper archives chats from the local Beeper Desktop API into the
// msgvault store. Each connected network account (whatsapp, signal, …)
// becomes its own msgvault source, so networks stay separately filterable
// while all rows share message_type "beeper".
//
// Import processes each chat in phases: backfill walks history oldest-ward
// with resumable, checkpointed cursors; incremental extends past the stored
// cursor; and reconcile re-walks the recent head to catch edits, deletions,
// and reaction changes a forward-only cursor cannot see. The client is
// GET-only by construction, so the archiver can never mutate Beeper state.
//
// Beeper message IDs are unique only per installation. Anchor probes
// (anchors.go) fingerprint the installation and are verified before any
// stored cursor is trusted: a reinstall or re-index aborts the run instead of
// silently duplicating the archive. Failed or over-cap media downloads leave
// pending marker rows that BackfillMedia retries later.
package beeper
