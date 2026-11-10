# Fictional Tumblr export fixtures

All HTML, handles, opaque IDs, text, dates, links and media bytes here were
independently authored for msgvault. No third-party export, message text,
identifier, avatar or CSS was copied. These authored fixtures are covered by
the repository license.

`observed-shape` uses the observed participant containers and labels, bare
conversation status heading, message-info header grammar, TEXT paragraphs and
POSTREF anchor shape. `overlap` repeats those fictional records in reverse
order under a different filename. `synthetic-media` invents IMAGE records and
standard HTML media, unsupported types, malformed headers, missing dates and
conflicting date evidence. These are edge cases, not observed official media
markup. The picture.txt file is fictional media bytes, not an actual image.

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
4bec837dd06d8028c0856f115c7768e2141936a59f158ab0a08bf047aed32c44  observed-shape/conversations/convo_700.html
7ac707c06039799f131e16bf1a0d8be88d4c4c6bb142f34bbc92bd305fbfee51  overlap/conversations/renamed.html
26fdb00d622527672191f10b6912f7b9830403bebf56645329c602f3a1435983  synthetic-media/conversations/convo_700.html
2aecb5f25ef82ad29edc5e67afc83e3b78ea9f6b4722e95ea510692fd5f4b87d  synthetic-media/media/picture.txt
```
