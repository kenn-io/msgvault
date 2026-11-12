# Fictional Instagram inbox JSON

Every display name, account label, message, timestamp and media byte in this
fixture was invented for msgvault. No user export, third-party fixture bytes or
parser code was copied. `chart.png` is a generated one-pixel amber image; the audio
reference deliberately has no file. Tests generate further fictional edge cases,
including generic media and empty/malformed URI references.

This fixture exercises the observed layout
`your_instagram_activity/messages/inbox/<thread>/message_<N>.json`, participants,
title, thread_path, sender_name, integer timestamp_ms, content, photos/videos/audio_files/files/gifs and generic `media` URI arrays,
share fields and reactions. It tests retained JSON inbox support, not an official
format version or complete real-account sent/received coverage.

Research on 2026-11-10 attempted the official
[Review and export a copy of your Instagram information](https://help.instagram.com/181231772500920)
help page (HTTP 429). The
[Facebook-hosted help URL](https://www.facebook.com/help/instagram/181231772500920)
redirected to login. No official versioned per-message JSON schema was verified.

Public parser references studied for structure only:

- [davidmalko87/whispergram](https://github.com/davidmalko87/whispergram),
  pinned commit `f7139f3595facb9d34f462b1600370531de67d60`;
  MIT License, copyright 2026 David Malko.
  Paths: `whispergram.py` (`_normalize_instagram`, `_ig_media_path`,
  `_fix_mojibake`) and `tests/test_whispergram.py` (`_write_ig_thread`,
  `test_normalize_instagram`). The Instagram evidence is an in-test synthetic
  builder; its on-disk `sample_export` is Telegram.
- [MasriAm/exodus-opensource](https://github.com/MasriAm/exodus-opensource),
  pinned commit `4adf4ac716114afea85871b2b0b45c0d4a4fb2b7`;
  MIT License, copyright 2026 MasriAm.
  Paths: `scripts/generate-fixture.ts`, generated `fixtures/demo-instagram.zip`,
  `parsers/instagram/index.ts`, `tests/core-layer-fixture-parsers.test.ts`.
  Its generated demo corroborates numbered inbox shards, `thread_path`,
  millisecond timestamps, media URIs and mojibake. Its `attachmentSchema` and
  `collectAttachments` corroborate the generic `media: [{uri: ...}]` shape.

These references establish the supported observed structure, not a complete
Instagram schema. HTML, ZIP, other sections/layouts and unavailable history are
outside this fixture's coverage. Stable IDs are msgvault fingerprints: no native
message ID, reply link or original timezone was observed in these records.
