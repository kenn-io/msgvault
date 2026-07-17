---
title: Analytics & Stats
description: Archive statistics, top senders, domains, and labels.
---


## Stats

Show overall archive statistics:

```bash
msgvault stats
```

Displays total message count, account breakdown, date range, storage size, and attachment count.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/stats.svg" alt="msgvault stats command output" loading="lazy">
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
  <img src="/assets/generated/list-senders.svg" alt="msgvault list-senders command output" loading="lazy">
</figure>
These commands query the configured daemon or remote server. For interactive exploration with drill-down, use the [TUI](/usage/tui/).
