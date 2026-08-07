# Documentation fixture format

The controlled web UI fixture is derived from the May 7, 2015 CMU CALO Enron
release. The complete upstream archive is a local selection input only. The
published `docs-fixtures` branch contains exactly:

- `enron-web-fixture.mbox.gz`
- `manifest.json`
- `README.md`

`manifest.json` records the source URL/release/checksum, selector version and
parameters, the `obvious-sensitive-markers-v1` pre-filter, sorted selected
participants from From/To/Cc/Bcc headers, the deterministic import owner
identifier, message and participant counts, the compressed MBOX checksum, the zero-attachment
constraint, attribution notes, and the manual review record. The pre-filter
only removes obvious credentials, phone/contact numbers, attachments, and
personal or financial markers before graph ranking; it is not a substitute for
the human review. Publication also rejects any retained MIME attachment so the
review surface remains text-only.
The selection record also stores sorted `excluded_message_ids` and
`excluded_addresses` arrays plus `exclusions_sha256`, which is the SHA-256 of
their canonical JSON form. This makes an exclusion part of reproducibility,
not an undocumented local choice.

Publication requires `manual_review.status` to be `complete`, with the exact
selected message count and MBOX checksum in `reviewed_message_count` and
`reviewed_fixture_sha256`. Any MBOX, selection parameter, or exclusion change
requires a fresh complete review.
