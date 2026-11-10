---
last_edited: 2026-11-10
---

# X DM fixture provenance

The two `direct-messages*.js` files retain one conversation each from public account-archive files. Every original string value was replaced: stable numeric IDs map to sequential synthetic IDs, text and names become synthetic values, links use example.com, and timestamps use a fixed synthetic instant. Object keys, event nesting, optional fields, and array shapes are retained. Original files and personal values are not retained. This documents observed layouts, not comprehensive X export coverage.

The public source repository, `larsbrinkhoff/twitter-data`, declares no repository-wide license at commit `bedf1fa56839650efa7cc704303a21bfb18bc833`. It supplies observed layout evidence, not permissively licensed synthetic test data. The fixtures retain structure with invented values; no source media is included.

- Direct source: https://github.com/larsbrinkhoff/twitter-data/blob/bedf1fa56839650efa7cc704303a21bfb18bc833/data/direct-messages.js (blob `98b3c6cdbbe7920d2e8ffedc23fc137984984faa`). The source contains 72 conversations and 900 messageCreate events. Selected excerpt: two messages and a URL object.
- Group source: https://github.com/larsbrinkhoff/twitter-data/blob/bedf1fa56839650efa7cc704303a21bfb18bc833/data/direct-messages-group.js (blob `8ff247ec6333b407b93aab469bb9496ba0ca4e58`). The source contains four conversations, four messageCreate events and four joinConversation events. Selected excerpt: a message and a participantsSnapshot membership event.

Synthetic owner `100001` occurs in both excerpts. Media arrays in the excerpts are empty; media tests must be described as synthetic. No deleted history, HTML-only layout, or newer encrypted chat format is evidenced by these files.

## Media reference evidence

`media-reference.js` is a further one-message excerpt from the same direct-message source. Every personal value was replaced. The `mediaUrls` value retains the producer's URL path shape with synthetic ID, token, and filename segments. Aggregate inspection found 12 media URLs in the source and 12 matching supplied filenames using the producer's `URL segment 4 + '-' + URL segment 6` rule under `data/direct_messages_media/`. No image or other media bytes were downloaded. A companion file for this fixture must contain synthetic bytes and be labeled accordingly. The redacted URL resolves to `100002-asset.png`.

Account metadata, numbered parts, containment cases and companion media bytes in tests are synthetic. The media-reference excerpt uses an independently redacted ID namespace; tests import it separately.

## Documentation and parser references

These references were checked on 2026-11-10. No third-party parser code or test fixtures were copied, and none is a dependency. Their licenses do not license the separate public account export above.

- [X archive download help](https://help.x.com/en/managing-your-account/how-to-download-your-x-archive) is the official general export-help entry point, not a versioned DM JSON schema. A focused search of the official help and developer documentation did not locate such a schema; Exa could not retrieve the help page. This is a research limit, not evidence that no official schema exists. No software or fixture license is attributed to this help page.
- [`yib7/twitter_GC_archival`](https://github.com/yib7/twitter_GC_archival/tree/56bf430940c57415d731a618485ff992a96d256a), commit `56bf430940c57415d731a618485ff992a96d256a`, is [MIT licensed](https://github.com/yib7/twitter_GC_archival/blob/56bf430940c57415d731a618485ff992a96d256a/LICENSE). Its [raw-export fixture builder](https://github.com/yib7/twitter_GC_archival/blob/56bf430940c57415d731a618485ff992a96d256a/tests/fixtures/x-export.js) explicitly declares IDs, text and media names invented. Its [ingestion tests](https://github.com/yib7/twitter_GC_archival/blob/56bf430940c57415d731a618485ff992a96d256a/tests/build-ingest.test.js) exercise the real streaming parser with those fixtures. This supplies a permissively licensed synthetic group-export reference, including numbered parts and media layout; its direct-message fixture tests exclusion from group ingestion, not direct-message import support.
- [`steipete/birdclaw`](https://github.com/steipete/birdclaw/tree/9895445ba0a24134cc6e07c327502134e18a58ea), commit `9895445ba0a24134cc6e07c327502134e18a58ea`, is [MIT licensed](https://github.com/steipete/birdclaw/blob/9895445ba0a24134cc6e07c327502134e18a58ea/LICENSE). Its [archive-import tests](https://github.com/steipete/birdclaw/blob/9895445ba0a24134cc6e07c327502134e18a58ea/src/lib/archive-import.test.ts) construct temporary archive inputs. Some identity labels reuse public identities, so these tests are not treated as wholly fictional fixture data and were not copied.
- [`alkihis/twitter-archive-reader`](https://github.com/alkihis/twitter-archive-reader/tree/a23fb890133553efa850b5c886ffbfb9a0892690), commit `a23fb890133553efa850b5c886ffbfb9a0892690`, is [CC BY-SA 4.0 licensed](https://github.com/alkihis/twitter-archive-reader/blob/a23fb890133553efa850b5c886ffbfb9a0892690/LICENSE). Its [layout notes](https://github.com/alkihis/twitter-archive-reader/blob/a23fb890133553efa850b5c886ffbfb9a0892690/Files_to_structures.md) and [media reader](https://github.com/alkihis/twitter-archive-reader/blob/a23fb890133553efa850b5c886ffbfb9a0892690/ts/MediaArchive.ts) are layout references only. Its [archive tests](https://github.com/alkihis/twitter-archive-reader/blob/a23fb890133553efa850b5c886ffbfb9a0892690/ts/test/archive.test.ts) require an author's local archive; they do not ship a synthetic export fixture.

Only `twitter_GC_archival` supplied an explicitly wholly fictional raw-export fixture reference in this focused search. These references do not establish coverage for other export generations or encrypted chat formats. The committed fixtures remain the redacted observations and locally invented cases described above.
