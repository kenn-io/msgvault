---
last_edited: 2026-11-10
---

# X DM fixture provenance

The two `direct-messages*.js` files retain one conversation each from public account-archive files. Every original string value was replaced: stable numeric IDs map to sequential synthetic IDs, text and names become synthetic values, links use example.com, and timestamps use a fixed synthetic instant. Object keys, event nesting, optional fields, and array shapes are retained. Original files and personal values are not retained. This documents observed layouts, not comprehensive X export coverage.

- Direct source: https://github.com/larsbrinkhoff/twitter-data/blob/bedf1fa56839650efa7cc704303a21bfb18bc833/data/direct-messages.js (blob `98b3c6cdbbe7920d2e8ffedc23fc137984984faa`). The source contains 72 conversations and 900 messageCreate events. Selected excerpt: two messages and a URL object.
- Group source: https://github.com/larsbrinkhoff/twitter-data/blob/bedf1fa56839650efa7cc704303a21bfb18bc833/data/direct-messages-group.js (blob `8ff247ec6333b407b93aab469bb9496ba0ca4e58`). The source contains four conversations, four messageCreate events and four joinConversation events. Selected excerpt: a message and a participantsSnapshot membership event.

Synthetic owner `100001` occurs in both excerpts. Media arrays in the excerpts are empty; media tests must be described as synthetic. No deleted history, HTML-only layout, or newer encrypted chat format is evidenced by these files.

## Media reference evidence

`media-reference.js` is a further one-message excerpt from the same direct-message source. Every personal value was replaced. The `mediaUrls` value retains the producer's URL path shape with synthetic ID, token, and filename segments. Aggregate inspection found 12 media URLs in the source and 12 matching supplied filenames using the producer's `URL segment 4 + '-' + URL segment 6` rule under `data/direct_messages_media/`. No image or other media bytes were downloaded. A companion file for this fixture must contain synthetic bytes and be labeled accordingly. The redacted URL resolves to `100002-asset.png`.

Account metadata, numbered parts, containment cases and companion media bytes in tests are synthetic. The media-reference excerpt uses an independently redacted ID namespace; tests import it separately.
