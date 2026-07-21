# Granola Date-Only Timestamp Fallback Design

## Problem

Granola normally returns RFC3339 timestamps, which decode directly into the
client models' `time.Time` fields. At least one valid note instead contains a
bare `YYYY-MM-DD` value. Go's standard JSON decoder rejects that value before
the importer can archive the note, so full syncs report a partial failure and
the note remains missing.

## Behavior

Granola timestamp fields continue to accept RFC3339 timestamps, including
fractional seconds and explicit offsets. If RFC3339 parsing fails, an exact
`YYYY-MM-DD` value is accepted and interpreted as `00:00:00Z` on that date.
Other malformed timestamp values still fail decoding.

The fallback applies consistently to every timestamp-bearing Granola API
field:

- note `created_at` and `updated_at`
- calendar `scheduled_start_time` and `scheduled_end_time`
- transcript segment `start_time` and `end_time`

## Implementation

Keep the exported Granola model fields as `time.Time` so the importer and its
time calculations retain their current interface. Add one package-local JSON
timestamp decoder that tries RFC3339 first and the date-only layout second.
Timestamp-bearing Granola wire structs use that helper while decoding.

The full-note decoder must explicitly combine its embedded note summary with
the remaining note fields so the summary's custom JSON behavior does not hide
the outer fields. Raw note archival remains unchanged: `GetNote` stores the
original response bytes in `Note.Raw` after typed decoding succeeds.

## Error Handling

The leniency is deliberately narrow. Only exact date-only strings gain a
fallback. Empty strings, partial timestamps, and other invalid values return a
contextual decode error and continue to fail that note rather than silently
inventing a timestamp.

## Testing

Client regression tests exercise the production JSON decoding paths:

- the list endpoint accepts date-only note summary timestamps as midnight UTC
- the full-note endpoint accepts date-only summary, calendar, and transcript
  timestamps as midnight UTC
- the full-note raw response remains byte-for-byte available for archival

Existing RFC3339 fixtures and importer tests continue to verify that normal
timestamp decoding and downstream behavior are unchanged.
