---
last_edited: "2026-09-08"
title: Archive Remote Email Images
description: Save images hosted by email senders for offline reading, with explicit tracking consent.
---

You can save externally hosted email images so you can read them later without
contacting the sender again. Remote image archiving is
**off by default** because downloading an image can trigger tracking pixels
and expose the archive server's IP address to the image host.

Normal mail archiving already stores embedded email attachments. Remote image
archiving handles images that a message loads from an external website.

## Archive images in existing mail

Start with a small scan of one source, using its source ID from
`msgvault list-accounts`:

```bash
msgvault archive-remote-images --allow-tracking --source-id 3 --limit 100
```

Here, `--limit` counts the messages scanned rather than the images downloaded.
To scan all email sources, omit `--source-id`. To scan without a cap, omit
`--limit`:

```bash
msgvault archive-remote-images --allow-tracking
```

You must pass `--allow-tracking` on every run, even if automatic archiving is
already turned on. When the command finishes, it reports counts for messages
scanned, images downloaded, images already archived, and errors. If some
downloads fail or you interrupt the run, msgvault keeps the images that
succeeded. Run the command again to retry the missing ones, and msgvault reuses
the images it already stored.

If you are running a large initial sync or import, finish importing your mail
before running this command. Otherwise, slow image hosts delay each message as
it comes in.

## Include images during future syncs and imports

To download remote images automatically going forward, enable
[`[sync].archive_remote_images`](/docs/configuration/#sync) in the daemon's
`config.toml` and restart the daemon:

```toml
[sync]
archive_remote_images = true
```

This setting covers Gmail and IMAP sync, along with EML, EMLX, Maildir, MBOX,
and PST imports. For messages already in your archive, use the backfill command
above. If you turn the setting off later, automatic downloads stop and images
you already stored stay in place.

During a Gmail sync, msgvault saves the message first and keeps unfinished
image downloads retryable if a run is interrupted or a download fails. It
reports pending image work in the sync's error count. On the next sync,
msgvault retries from the local archive without downloading the message from
Gmail again. While automatic downloads are turned off, that work stays pending
and msgvault does not contact any image host.

## Read and export the archive

The Web UI's message and conversation views show local copies when they are
available. Any image that was not archived still follows the reader's
remote-image consent control. If an archived image's local file is missing,
opening it does not fetch a replacement from the remote host.

msgvault stores downloaded images as inline attachments. They count toward
your attachment totals and show up in the **Files** view, including small logos
and tracking pixels. To save their bytes, use the standard
[attachment export commands](/docs/usage/exporting/).

The original raw MIME and stored HTML do not change. Because of that,
`export-eml` preserves the original message and its original image URLs, and
does not embed the newly archived images into the exported EML file.

## Coverage and limits

Remote image archiving reads HTTP(S) `<img src>` URLs, including URLs that
start with `//`. It stores PNG, JPEG, GIF, and WebP images. It skips SVG, CSS
backgrounds, `srcset` alternatives, external stylesheets, and linked pages.

| Limit | Value |
|---|---|
| Distinct image URLs per message | 64 |
| Each image | 10 MiB |
| Total newly downloaded image bytes per message | 30 MiB |
| HTML message body processed | 8 MiB |
| Time per image fetch | 15 seconds |
| Time budget per message | 60 seconds |
| Redirects per image | 3 |

msgvault can still archive a message even when an image exceeds a limit, is
unavailable, or fails to download. Gmail sync counts the error and keeps the
unfinished work for retry, while other imports report image errors separately
from mail-import results. Old sender URLs may have expired already, and
msgvault cannot rebuild an image that the host no longer serves.
