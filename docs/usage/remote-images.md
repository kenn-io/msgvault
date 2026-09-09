---
last_edited: "2026-09-08"
title: Archive Remote Email Images
description: Save images hosted by email senders for offline reading, with explicit tracking consent.
---

Save externally hosted email images so you can read them later without
contacting the sender again. Remote image archiving is **off by default**:
downloading an image can activate tracking pixels and disclose the archive
server's IP address to the image host.

Embedded email attachments are already part of normal mail archiving. This
feature covers images that the message asks a website to load.

## Archive images in existing mail

Start with a small scan of one source. Use the source ID from
`msgvault list-accounts`:

```bash
msgvault archive-remote-images --allow-tracking --source-id 3 --limit 100
```

`--limit` counts messages scanned, not images downloaded. Omit `--source-id`
to scan all email sources; omit `--limit` for an unlimited scan:

```bash
msgvault archive-remote-images --allow-tracking
```

Every invocation requires `--allow-tracking`, even if automatic archiving is
already enabled. The command reports messages scanned, images downloaded,
images already archived, and errors. Successful downloads remain stored if
other downloads fail or you interrupt the command. Rerun it to retry missing
images; stored images are reused.

For a large initial sync or import, finish importing mail before running this
command. Slow image hosts otherwise delay each message as it is ingested.

## Include images during future syncs and imports

Enable [`[sync].archive_remote_images`](/docs/configuration/#sync) in the
daemon's `config.toml`, then restart the daemon:

```toml
[sync]
archive_remote_images = true
```

This enables downloads during Gmail and IMAP sync and EML, EMLX, MBOX, and PST
imports. Use the backfill command above for messages already archived.
Disabling the setting stops automatic downloads and keeps images already
stored.

## Read and export the archive

The Web UI's message and conversation views use local copies when available.
Images that were not archived still follow the reader's remote-image consent
control. Opening an archived image does not fetch a replacement from its host
if its local file is missing.

Downloaded images are inline attachments. They count toward attachment totals
and appear in the Files view, including small logos and tracking pixels. Use
the normal [attachment export commands](/docs/usage/exporting/) to save their
bytes.

The original raw MIME and stored HTML stay unchanged. An `export-eml` therefore
preserves the original message, including its original image URLs; it does not
embed the newly archived images into that EML file.

## Coverage and limits

Archiving reads HTTP(S) `<img src>` URLs, including URLs beginning with `//`.
It stores PNG, JPEG, GIF, and WebP images. It does not download SVG, CSS
backgrounds, `srcset` alternatives, external stylesheets, or linked pages.

| Limit | Value |
|---|---|
| Distinct image URLs per message | 64 |
| Each image | 10 MiB |
| Total newly downloaded image bytes per message | 30 MiB |
| HTML message body processed | 8 MiB |
| Time per image fetch | 15 seconds |
| Time budget per message | 60 seconds |
| Redirects per image | 3 |

A message can be archived even when an image exceeds a limit, is unavailable,
or fails to download. Review image errors separately from mail-import results.
Old sender URLs may already have expired; msgvault cannot reconstruct an image
that the host no longer serves.
