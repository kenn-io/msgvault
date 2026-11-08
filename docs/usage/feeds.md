---
last_edited: "2026-11-07"
title: RSS, Atom and JSON Feeds
description: Follow feeds and keep every item msgvault has seen, searchable next to your mail and chats.
---

Follow the blogs, release pages, and other feeds you read, and keep every item
searchable next to your mail and chats. msgvault fetches the feeds itself, so
you need no feed reader or account, and it keeps items after a feed drops
them.

## Follow feeds

Pass each feed's URL, or the OPML file a feed reader exports:

```bash
msgvault add-feed https://example.com/releases.atom
msgvault add-feed --opml ~/Downloads/subscriptions.opml
```

`add-feed` fetches each URL first. A URL that is not a reachable RSS 2.0, Atom,
or JSON feed is reported and not followed. The items a feed lists now are
archived right away. `--opml` follows every feed in the file without fetching
them; the next sync fetches them.

## Keep feeds current

Fetch followed feeds on a schedule with the `[feeds]` block in `config.toml`:

```toml
[feeds]
enabled = true
schedule = "*/30 * * * *"
```

Run `msgvault sync-feeds` to fetch them now. Once the schedule is set, the
Sources page offers **Sync now** for the Feeds source.

- An unchanged feed isn't downloaded again. `sync-feeds --full` downloads
  every feed again.
- Each sync fetches the least recently checked feeds first, so a scheduled
  sync that pauses for other work picks up the feeds it didn't reach.
- A feed that fails is recorded on the sync run, and the other feeds still
  sync. `msgvault list-feeds` shows each feed's last check and its last error.
  A sync counts as failed, and the Sources page keeps its last good sync,
  only when every feed fails.

## Feeds on your own network

A feed whose URL names a private address or a local name, such as
`http://192.168.1.20/feed.xml` or `http://nas.local/feed.xml`, is fetched from
your network. A feed whose URL names a public host may only reach public
addresses: a redirect, or a host name that resolves to a private, loopback or
reserved address, fails the fetch, so a publisher can't send msgvault to your
own machine or network. To follow an internal feed whose host name resolves to
a private address, add it by that address.

## What gets archived

All feeds belong to one source, **Feeds** (type `feed`). Each feed is a
conversation named after the feed, and each item is a message of type
`feed_item`:

```bash
msgvault search "release notes" --message-type feed_item
```

| Item part | Where it goes |
|---|---|
| Title | The message subject |
| Content, or the summary when there is none | The message body. HTML is kept, with a plain-text form for search and previews; plain text, such as JSON Feed `content_text` or Atom text content, is kept as text |
| Link | The end of the body; relative links and images in the HTML are made absolute |
| Published time, else updated time | The message date; an item without one is dated when first seen |
| Categories, enclosures, and image | The message's metadata |

Each item is archived once, keyed by its RSS `guid`, Atom `id`, or JSON Feed
`id`, else by its link, within its feed: feeds promise unique IDs only within
themselves, so an item that two feeds publish is archived in each. A later
edit to an item does not change the archived copy.

The sender follows the item's author. An author with a URL is that site's
participant. An author with only a name is a participant of that feed alone,
so two feeds' "Admin" stay two people. An item without an author is from the
feed's site. Author emails are ignored: many feeds publish one placeholder
address for every author.

## Stop following a feed

```bash
msgvault remove-feed https://example.com/releases.atom
```

Archived items stay searchable. `add-feed` with the same URL follows it again.
To delete every feed item instead, run
`msgvault remove-account --type feed feeds`, which cannot be undone.

## Limitations

- Give the feed's own URL. A website's home page is not searched for its feed.
- Feeds that need a login are not supported.
- msgvault archives items; it does not track which ones you have read.
