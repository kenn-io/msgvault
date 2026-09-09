# Engineering records

These documents preserve design rationale, investigations, and implementation
plans. They are outside the public documentation build and are not current
operating instructions. A recorded proposal or task list does not establish
that a feature is implemented, approved, or scheduled.

For the current system, start with [architecture](../architecture/overview.md).
For maintenance rules, see the [documentation contributor guide](../README.md).

## Find the original rationale

| Topic | Engineering records | Current documentation |
|---|---|---|
| Accounts, identities, collections, and duplicate handling | [Design set](accounts-identities-collections-dedup/README.md) | [Accounts](../usage/multi-account.md), [people](../usage/people.md), [deduplication](../usage/deduplication.md) |
| Attachment packs and restore | [Packed attachments](packed-attachments-design.md), [pack extraction](kit-packstore-extraction-design.md), [native restore](pack-native-restore-design.md) | [Storage](../architecture/storage.md), [backup format](../architecture/backup-format.md) |
| Slack ingestion and reply discovery | [Ingestion](slack-ingestion-design.md), [reply sweep](slack-reply-sweep-design.md) | [Slack](../usage/slack.md) |
| Message exports | [Design](message-export-design.md) and [plan](message-export-plan.md) | [Exporting](../usage/exporting.md) |
| People and relationships | [Relationship index](relationship-list-index-design.md), [merge reversal](person-merge-reversal.md), [conversation brief](last-time-we-talked-design.md) | [People and profiles](../usage/people.md) |
| Daemon command routing | [CLI audit](daemon-cli-request-audit.md) | [Daemon guide](../guides/daemon-migration.md) |
| PostgreSQL | [Original implementation tracker](PG_STATUS.md) | [PostgreSQL backend](../architecture/postgresql.md) |
| Recovery | [Recovery notes](recovery.md) | [Backup](../usage/backup.md) and [troubleshooting](../troubleshooting.md) |

Retain recorded approvals, unresolved decisions, and exception-removal
conditions when updating these records. Consult current source and the owning
guide before acting on an old command, protocol shape, or implementation plan.
