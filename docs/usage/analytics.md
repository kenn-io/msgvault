---
last_edited: "2026-09-22"
title: Analytics & Stats
description: Archive statistics, top senders, domains, and labels.
---

Use the built-in aggregate commands for a quick archive summary. They query the
configured remote server when one is set; otherwise they use the local daemon.

## Stats

Show overall archive statistics:

```bash
msgvault stats
```

The output includes message, thread, attachment, label, and account counts plus
the database size. Source-deleted messages are reported separately when any
exist. Scope the counts to one account or collection when needed:

```bash
msgvault stats --account you@example.com
msgvault stats --collection work
```
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/stats.svg" alt="msgvault stats command output" loading="lazy">
</figure>
## List Senders

```bash
# Top 20 senders
msgvault list-senders --limit 20
```

## List Domains

```bash
# Top 20 sender domains
msgvault list-domains --limit 20
```

## List Labels

```bash
# All labels with message counts
msgvault list-labels
```
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/list-senders.svg" alt="msgvault list-senders command output" loading="lazy">
</figure>
For interactive exploration, use the [Web UI](/docs/web-ui/) to combine search,
filters, and grouping, then share a link to that view. Use the
[TUI](/docs/usage/tui/) to explore the same archive from the terminal.
