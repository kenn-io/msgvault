---
last_edited: 2026-11-06
title: YouTube transcripts
description: Attach official caption text to archived YouTube videos.
---

# Attach transcripts to YouTube videos

msgvault can attach a transcript to each archived video on your configured
YouTube channel. It uses the official caption API with your existing YouTube
credentials. It does not download video files, scrape YouTube, or download
auto-generated ASR tracks.

First [configure and register the channel](youtube.md). The token needs the
`youtube.force-ssl` scope and permission to edit the video's captions.

## Enable captions

Captions are off by default. Set these fields on the existing profile to
include them in manual and scheduled syncs:

```toml
[[youtube]]
identifier = "example-channel"
channel_id = "UCexample000000000000000"
token_env = "EXAMPLE_YOUTUBE_REFRESH_TOKEN"
client_id_env = "EXAMPLE_YOUTUBE_CLIENT_ID"
client_secret_env = "EXAMPLE_YOUTUBE_CLIENT_SECRET"
enabled = true
schedule = "*/30 * * * *"
captions = true
caption_units = 2000
```

An omitted or zero `caption_units` uses 2,000 units. Negative values are
invalid. For one manual run, enable captions or override the budget with:

```bash
msgvault sync-youtube example-channel --captions --caption-units 2000
```

These flags change only that invocation. `--captions=false` turns captions
off for that run. `--caption-units` must be positive; setting it does not
enable captions by itself.

## Quota and progress

Caption work has its own saved cursor and quota budget. Listing tracks costs
50 units; downloading one costs 200. The comment request limit does not
consume this budget. A comment run that reaches its request limit still
allows caption work. Channel authorization and provider quota failures stop
further network requests.

When the remaining budget cannot cover a selected download, msgvault saves
that track and resumes it on the next sync without listing it again. A
partial caption run reports `partial=true` without failing the sync. Budgets
under 200 cannot finish a pending download; increase the budget to continue.

Each caption cycle visits a finite snapshot of archived video roots in order.
New uploads join the next cycle, so they cannot keep older videos from
getting a turn. Missing, ASR, non-serving, inaccessible and unconvertible tracks
get an unavailable observation with a timestamp. Later cycles check them again.
Malformed or oversized captions remain visible as errors while other videos
advance; they are retried on a later cycle. Responses are limited to 16 MiB.

## Stored text and Docbank

msgvault selects the first serving non-ASR track ordered by language and
track ID. It stores a `text/plain` attachment on the video's existing root
message. Timing, cue identifiers, styles and caption markup are removed;
speech stays in cue order. The original VTT is not stored.

An unchanged track version is not downloaded again. A changed version updates
the same attachment occurrence. A different selected track has its own
occurrence; previously stored text is preserved when a track disappears.

The normal attachment mirror discovers these transcripts, including text
added after a video was archived. Docbank receives them only when its
integration and attachment upload consent are already enabled. Caption sync
does not contact Docbank directly or change that consent.
