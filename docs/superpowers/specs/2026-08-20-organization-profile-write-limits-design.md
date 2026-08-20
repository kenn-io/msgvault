# Organization Profile Write Limits

## Problem

Organization profile replacement currently limits the encoded HTTP request to
12 MiB but does not limit the number of collection values or the total inline
media represented by retention hashes. A valid request can therefore exceed a
database driver's bind-parameter limit or expand a small request into an
unbounded amount of retained media work.

## Goals

- Bound the work and active inline media produced by one full-profile write.
- Preserve normal GET-derived PUT behavior for retained media.
- Return a stable client error before a partial profile update can commit.
- Keep the SQLite and PostgreSQL implementations on the same code path.

This change will not introduce a new media storage schema or deduplicate blobs
across rows. Those changes require a separate migration and lifecycle design.

## Limits

An organization profile may contain at most 200 values in total across names,
identifiers, addresses, contact points, media, and categories. Each inline
media value remains limited to 8 MiB. The sum of inline bytes represented by
the desired active media rows may not exceed 32 MiB, including bytes recovered
through `content_hash` retention.

The limits are strict. A profile written before this change that exceeds a
limit cannot be replaced unchanged; the caller must reduce it to an accepted
size.

## Store Design

The store will expose shared organization-profile limit constants and a typed
`ErrOrganizationProfileTooLarge` error. A common validator will count all
collection values and explicit media bytes at the start of the store
preparation path, so direct store callers cannot bypass the limits without
duplicating validation in the HTTP conversion path.

Retained media bytes are only known after the store finds the active source
rows. Retention resolution will start with the explicit-byte total, add the
resolved byte length for every desired media row, and return the same typed
error if the 32 MiB total would be exceeded. The surrounding transaction will
roll back the revision bump and all profile changes.

Retention resolution will cache data by content hash. Each distinct stored
blob will be queried once, and inputs that retain the same blob will share the
loaded byte slice. The byte budget still counts every desired row because each
row represents separately stored active media.

The 200-value cap also bounds every retained-ID list used by collection
reconciliation. Existing `NOT IN` statements will remain safely below the
database parameter ceiling, so this change does not need dialect-specific SQL
or a temporary table.

## HTTP Contract

The organization profile PUT endpoint will map
`ErrOrganizationProfileTooLarge` to HTTP 413 with the existing
`organization_profile_too_large` error code. Its OpenAPI operation will declare
the 413 response and describe the aggregate value limit. The API schema version
will advance to distinguish the updated contract. Other profile validation
errors will retain their current status and response codes.

## Tests

Store tests will prove that:

- 200 aggregate values can be written and replaced without row churn.
- 201 aggregate values fail before the organization revision changes.
- explicit media whose logical total exceeds 32 MiB is rejected.
- repeated retention hashes whose expanded total exceeds 32 MiB are rejected
  atomically while the original media remains readable.

API tests will prove that an over-limit profile receives HTTP 413 with the
expected error code. OpenAPI verification will cover the new documented
response. Focused package tests, the complete tagged Go suite, formatting,
vetting, and linting will run before the branch is published.
