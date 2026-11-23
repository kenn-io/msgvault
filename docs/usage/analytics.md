---
last_edited: "2026-09-22"
title: Analytics & Stats
description: Archive statistics, top senders, domains, and labels.
---

Use the built-in aggregate commands to get a quick summary of your archive. If
you have a remote server configured, these commands query it; otherwise, they
talk to your local daemon.

## Stats

To see overall statistics for your archive, run:

```bash
msgvault stats
```

The output reports counts for messages, threads, attachments, labels, and
accounts, along with the total database size. If your archive contains
source-deleted messages, `msgvault stats` reports them separately. To narrow the
counts to a single account or collection, pass `--account` or `--collection`:

```bash
msgvault stats --account you@example.com
msgvault stats --collection work
```
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/stats.svg" alt="msgvault stats command output" loading="lazy">
</figure>
## List senders

```bash
# Top 20 senders
msgvault list-senders --limit 20
```

## List domains

```bash
# Top 20 sender domains
msgvault list-domains --limit 20
```

## List labels

```bash
# Labels with message counts (up to 50 by default)
msgvault list-labels
```
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/list-senders.svg" alt="msgvault list-senders command output" loading="lazy">
</figure>
If you want to explore your archive interactively, open the
[Web UI](/docs/web-ui/) to combine search, filters, and grouping, then share a
link to your view. You can also browse the same archive from your terminal in
the [TUI](/docs/usage/tui/).
