# Reversible person merges

Person merges combine two curated profiles without treating either side as
disposable. The selected survivor keeps its person ID and vCard UID. The
absorbed person is deleted only after its bindings and profile rows have been
reconciled inside the same transaction. A split creates a new person and UID;
historical IDs and retired UIDs are never reused.

## Durable state

Each merge stores two related forms of history:

- `person_merges.snapshot_blob` is the SHA-256-verified, canonical snapshot of
  both roots and every registered row needed to interpret the merge. This is
  the canonical audit payload; normal merge and split operations do not
  rewrite it.
- `person_merge_rows` is operational undo state. Later merges and splits can
  rebase its current row locators and dispositions while preserving the
  snapshot's audited meaning.
- `person_merge_participants` records survivor and absorbed lineage. Split IDs
  close selected lineage, and `current_person_id` becomes `NULL` when a merge
  has no remaining absorbed lineage.

Exact reversal means the selected lineage and live dependencies still permit
the pre-merge profiles to be restored. Partial splits move only attributable
rows and report ambiguous or unrestored rows. The implementation must not call
a split exact when any required row was skipped.

## Lifecycle boundaries

- Active merge lineage prevents deletion of the current person. A completed
  split releases that guard.
- A profile with an active CardDAV publication cannot participate in a merge.
  Merging it locally would otherwise change identity while an external address
  book still owns publication state for the old profile.
- Snapshots retain merge-time profile values after later live edits or
  redaction. Complete subset packets therefore require attributes, profiles,
  and native vCard resources together, and the subset command warns that the
  packet contains historical personal data.
- There is no separate purge operation. Removing durable history would also
  remove the evidence needed to inspect or reverse the merge.

## Table-registry invariant

`personMergeTableRegistry` is the closed inventory of direct and polymorphic
references to `persons`. Merge code must classify every such reference before
the absorbed root is deleted. The inventory test reads the live SQLite and
PostgreSQL catalogs and compares every direct foreign key with the registry;
known polymorphic references are asserted explicitly.

A change that adds a person reference must update the registry and any
table-specific merge or restore semantics in the same change. Both inventory
tests must pass before that schema can ship.

## Schema-migration rule

Snapshot rows record their column names and values. Restore statements use
those recorded columns, so a migration that renames or removes one of them can
make historical packets impossible to replay. Adding a required column with no
database default can also prevent recreation of a deleted snapshot row.

Before changing a snapshotted table, the migration must choose and verify one
forward path:

1. Keep old snapshot columns and row insertion compatible with the new schema.
2. Transform stored snapshots and journal JSON once during migration, preserve
   their meaning, and recompute each snapshot hash.

The migration must exercise a merge created with the pre-migration schema and
split it after the upgrade on SQLite and PostgreSQL. Do not add a permanent
dual-read fallback for an obsolete packet shape.
