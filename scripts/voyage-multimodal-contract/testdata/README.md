---
last_edited: 2026-08-13
---

# Voyage multimodal contract fixtures

The authenticated contract tests generate their JPEG, PNG, WebP, animated GIF,
and MP4 inputs at runtime. No customer media, API responses, request bodies, or
embedding vectors are stored here.

WebP and MP4 generation requires `ffmpeg`; the tests fail closed when an API key
is present but the fixture generator is unavailable.
