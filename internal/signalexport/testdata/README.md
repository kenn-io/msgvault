# Fictional native-v1 fixture

`native-v1` is independently authored fictional data for the approved
fictional-fixture route. It is not a capture from a released Signal app.
Names, identifiers, text and media bytes are invented. No external code,
fixture bytes or personal identifiers were copied. Tests derive further
fictional variants for edit ancestry, local ID renumbering and unsafe media.

Format facts were researched from these exact official sources, licensed
AGPL-3.0-only. They were used as format documentation, without copying
implementation or fixtures:

- Signal Desktop commit `9173731aaaccec8444066aec0e0c78725d7e457b`:
  [export entry point](https://github.com/signalapp/Signal-Desktop/blob/9173731aaaccec8444066aec0e0c78725d7e457b/ts/services/backups/index.preload.ts),
  [JSON encoding](https://github.com/signalapp/Signal-Desktop/blob/9173731aaaccec8444066aec0e0c78725d7e457b/ts/services/backups/encoding.node.ts),
  [edit timestamps](https://github.com/signalapp/Signal-Desktop/blob/9173731aaaccec8444066aec0e0c78725d7e457b/ts/services/backups/export.preload.ts),
  [plaintext filenames](https://github.com/signalapp/Signal-Desktop/blob/9173731aaaccec8444066aec0e0c78725d7e457b/ts/services/backups/util/mediaId.preload.ts),
  [media export](https://github.com/signalapp/Signal-Desktop/blob/9173731aaaccec8444066aec0e0c78725d7e457b/ts/jobs/AttachmentLocalBackupManager.preload.ts),
  [directory layout](https://github.com/signalapp/Signal-Desktop/blob/9173731aaaccec8444066aec0e0c78725d7e457b/ts/services/backups/util/localBackup.node.ts).
- libsignal commit `850942581c06fa65c0323fd52029afa02fa1a1ba`:
  [schema and ordering](https://github.com/signalapp/libsignal/blob/850942581c06fa65c0323fd52029afa02fa1a1ba/rust/message-backup/src/proto/backup.proto),
  [JSON exporter and exclusions](https://github.com/signalapp/libsignal/blob/850942581c06fa65c0323fd52029afa02fa1a1ba/rust/message-backup/src/json/exporter.rs).
- Android commit `515db93fb48206d63406cbfb48c5816f6af1d9e2`:
  [LocalArchiver](https://github.com/signalapp/Signal-Android/blob/515db93fb48206d63406cbfb48c5816f6af1d9e2/app/src/main/java/org/thoughtcrime/securesms/backup/v2/local/LocalArchiver.kt).
- iOS commit `012f2f29307dd084476c641968e382f25cb76737`:
  [PlaintextTakeoutManager](https://github.com/signalapp/Signal-iOS/blob/012f2f29307dd084476c641968e382f25cb76737/SignalServiceKit/Backups/PlaintextTakeout/PlaintextTakeoutManager.swift).

CC0-1.0 structural research: emptyname-org/chat-extractor commit
`1820d38a5290a6973a548a3c82d51777ab74fcae`,
[synthetic fixture construction](https://github.com/emptyname-org/chat-extractor/blob/1820d38a5290a6973a548a3c82d51777ab74fcae/tests/core.rs)
and [license](https://github.com/emptyname-org/chat-extractor/blob/1820d38a5290a6973a548a3c82d51777ab74fcae/LICENSE).
Its older media naming and noncanonical scalar/locator assumptions were not
adopted. Current media uses the plaintext SHA-256 hex stem in `files/<prefix>/`,
with an optional extension. Keys do not participate in that filename.

Source presence and fictional tests do not establish runtime compatibility
with released-app exports or equivalent coverage across platforms.
