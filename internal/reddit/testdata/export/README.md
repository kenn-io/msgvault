# Fictional Reddit account export

Every handle, ID, timestamp, IP, URL and content value in these CSVs was invented
for msgvault tests. No third-party rows or implementation were copied. The IP
uses a documentation range and URLs use example.com. This is a schema fixture,
not a real or redacted account export and not evidence of complete lifetime
coverage.

Structural research recorded 10 November 2026:

- Reddit's official [file inventory](https://support.reddithelp.com/hc/en-us/p/what_is_in_my_reddit_data_copy)
  documents Chat, PM, post, comment and statistics CSVs, without column schemas.
- A [primary export-viewer author's layout report](https://peakappsstudio.com/reddit-data-export-viewer/what-is-in-the-export/)
  describes an export made June 2026 (last checked 29 October 2026): UTF-8,
  quoted multiline cells, header-only files, numbered splits, explicit UTC
  timestamps and media represented by links. This is format evidence, not a
  licensed fixture.
- MIT [xavdid/reddit-user-to-sqlite](https://github.com/xavdid/reddit-user-to-sqlite)
  at `e02fad746694f32ebc2ee2efce82652857682cc6`: `tests/conftest.py` generates
  minimal fictional ID-only CSVs; `tests/test_csv_helpers.py` tests CSV reading.
  Its statistics reader uses `account name`. Other API objects were not reused.
- MIT [andrewbanchich/shreddit](https://github.com/andrewbanchich/shreddit)
  at `30f98fe22f68d8357fa28c490b660a6133f348b4`: `src/sources/gdpr.rs`;
  no fixture files.
- AGPL-3.0 [guilamu/reddit-gdpr-export-viewer](https://github.com/guilamu/reddit-gdpr-export-viewer)
  at `e3c335574c1ea93eecd3475323b676ea4389876d`: `app.js` recognizes
  `messages_archive.csv`; no test fixtures. Structural facts only.
- AGPL-3.0 [EpicenterHQ/epicenter](https://github.com/EpicenterHQ/epicenter)
  at `e6c5d0a3ac8786788eeb58fc1e19fd23330a621c`: `apps/reddit/LICENSE` and
  `apps/reddit/src/lib/workspace/ingest/reddit/{csv-schemas.ts,parse.test.ts,index.test.ts,README.md}`
  corroborate headers, both PM filenames, numbered CSVs, BOMs and empty files.
  Structural facts only; current main has removed that app.

Fixture coverage includes all four kinds, PM aliases and same-file duplicates,
multiline text, empty files, split files, unknown dates/senders and extra
metadata. Tests author additional malformed and unsafe inputs in temporary
directories. Absent columns differ from empty cells in archived raw JSON.
Chat channel and participant semantics beyond observed fields remain unknown.
No link is fetched, media downloaded, or removed history recovered. The separate
[legacy PM archive UI](https://support.reddithelp.com/hc/en-us/articles/39673186812564-Accessing-existing-private-messages-private-message-archive)
limit does not establish a limit on account exports.
