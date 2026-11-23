---
last_edited: "2026-11-12"
title: RSS, Atom and JSON Feeds
description: Follow feeds and keep every item msgvault has seen, searchable next to your mail and chats.
---

Follow the blogs, release pages, and other feeds you read, and keep every item
searchable alongside your mail and chats. msgvault fetches feeds directly, so
you do not need a separate feed reader or account, and it keeps items after a
feed drops them.

## Follow feeds

Pass each feed's URL, or pass an OPML file exported from a feed reader:

```bash
msgvault source feeds add https://example.com/releases.atom
msgvault source feeds add --opml ~/Downloads/subscriptions.opml
```

When you pass feed URLs, `source feeds add` fetches each URL first and archives
the items currently listed in the feed right away. If a URL is not a reachable
RSS 2.0, Atom, or JSON feed, msgvault reports the error and does not follow it.
When you pass `--opml`, msgvault follows every feed in the file without fetching
them immediately, and the next sync fetches their items.

## Keep feeds current

To fetch followed feeds on a schedule, add a `[feeds]` block to `config.toml`:

```toml
[feeds]
enabled = true
schedule = "*/30 * * * *"
```

To fetch feeds immediately, run `msgvault source feeds sync`. After you set a
schedule, the **Sources** page shows **Sync now** for the **Feeds** source.

- If a feed has not changed since the last check, msgvault does not download
  it again. To download every feed again, run `source feeds sync --full`.
- Each sync starts with the least recently checked feeds, so if a scheduled
  sync pauses for other work, the next run picks up the feeds it did not reach.
- If a feed fails, msgvault records the error on the sync run and continues
  syncing the other feeds. Run `msgvault source feeds list` to see each feed's
  last check and last error. A sync counts as failed, and the **Sources** page
  keeps its last good sync, only when every feed fails.

## Feeds on your own network

When a feed URL uses a private address or a local name, such as
`http://192.168.1.20/feed.xml` or `http://nas.local/feed.xml`, msgvault fetches
it from your network. A feed URL with a public host can only reach public
addresses. If a public host redirects or resolves to a private, loopback, or
reserved address, the fetch fails, so a publisher cannot direct msgvault to
your own machine or network. To follow an internal feed whose hostname resolves
to a private address, add it using that address.

## What gets archived

All feeds belong to one source, **Feeds** (type `feed`). Each feed becomes a
conversation named after the feed, and each item becomes a message of type
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

msgvault archives each item once per feed, identifying it by its RSS `guid`,
Atom `id`, or JSON Feed `id`, or by its link if no ID is present. Feeds
guarantee unique IDs only within themselves, so if two feeds publish the same
item, msgvault archives a copy in each. Later edits to an item do not change
the archived copy.

The sender comes from the item's author. If an author includes a URL, msgvault
treats them as a participant for that site. If an author has only a name,
msgvault scopes that participant to the feed, so an "Admin" in two different
feeds stays two separate people. Items without an author come from the feed's
site. msgvault ignores author email addresses. Many feeds publish one
placeholder address for every author.

## Stop following a feed

To stop following a feed while keeping its archived items searchable, remove
its URL:

```bash
msgvault source feeds remove https://example.com/releases.atom
```

To follow the feed again later, run `source feeds add` with the same URL. To
delete every archived feed item instead, run
`msgvault remove-account --type feed feeds`. This action cannot be undone.

## Limitations

- Provide the feed's direct URL. msgvault does not search a website's home page
  for a feed.
- Feeds that require a login are not supported.
- msgvault archives items and does not track which ones you have read.
