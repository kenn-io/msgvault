# Fictional Tumblr export fixtures

All HTML, handles, opaque IDs, text, dates, links and media bytes here were
independently authored for msgvault. No third-party export, message text,
identifier, avatar or CSS was copied. These authored fixtures are covered by
the repository license.

`observed-shape` uses the observed participant containers and labels, conversation
status followed by the `Participants:` heading, message-info header grammar,
TEXT paragraphs and POSTREF anchors. Tests derive a reordered copy under a
different filename to exercise overlap. This order is synthetic; observed
exports are newest-first. `synthetic-media` invents IMAGE records and standard
HTML media, unsupported types, malformed headers, missing dates and conflicting
date evidence. These are edge cases, not observed official media markup. The
missing-ID fallback is also synthetic, not observed export structure.
The picture.txt file is fictional media bytes, not an actual image.

## Structural provenance

- Official export documentation: https://help.tumblr.com/knowledge-base/export-your-blog/
  confirms per-blog ZIP, HTML conversations and uploaded messaging media. It
  does not define markup, received-media retention or URL expiry.
- Public structural specimen: https://github.com/extratone/theautobully
  revision `7dac45f3849c952749d7defb0e913202477ed031`, **no declared license**.
  Only tags, labels and grammar were studied. No files or content were copied.
  Studied files (URLs retained solely as provenance; no contents copied):
  - https://github.com/extratone/theautobully/blob/7dac45f3849c952749d7defb0e913202477ed031/conversations/convo_243215051_asphaltapostle_theautobully-blog.html
  - https://github.com/extratone/theautobully/blob/7dac45f3849c952749d7defb0e913202477ed031/conversations/convo_437014265_tumblrbot_theautobully-blog.html
- Legacy parser investigated: https://github.com/tiyb/tev
  revision `6db52f0f3587b06493ee3a2ef4f7c31b3ebee318`, MIT. Its XML parser does
  not establish current HTML structure; no code or fixtures reused.
- Export tool investigated: https://github.com/trevoedwards/tumbl
  revision `34a0af07434a8b24361b53c4124c1b31b97c7fcd`, MIT. Conversations are
  out of scope; no code or fixtures reused.

## Authored fixture SHA-256 digests

Digests cover each authored file relative to this directory.

```text
6a7fb7063d3e9b37e4278540f7f79a50e48a65cca746c6ad89be13b73882a2bf  observed-shape/conversations/convo_700.html
668fb851023e72af9b668e46d49a86190cca2e732d6dac4754cd1a0975e40377  synthetic-media/conversations/convo_700.html
2aecb5f25ef82ad29edc5e67afc83e3b78ea9f6b4722e95ea510692fd5f4b87d  synthetic-media/media/picture.txt
```
