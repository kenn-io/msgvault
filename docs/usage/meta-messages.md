---
last_edited: "2026-11-10"
title: Facebook and Instagram Messages
description: Archive a Facebook Page's Messenger inbox or a linked Instagram account's direct messages as searchable chats.
---

Search the conversations customers have with your Facebook Page on Messenger,
or with your professional Instagram account in Instagram direct messages,
beside your other chats. msgvault reads these inboxes with the Page token; it
does not send messages.

This archives a **Page's** or business account's inbox. To archive your
personal Messenger history, import a Facebook export with
[`import-messenger`](text-messages.md#import-messenger). The two never share
conversations or people: a personal export and a Page inbox use different
identities, so a chat between your own account and your Page appears once in
each source.

## Connect an inbox

1. Use a Page access token for the Page, with the messaging permissions your
   Meta app was granted. Keep the token in a named environment variable on the
   **daemon host**.
2. Add a profile to the [configuration](../configuration.md#meta-inboxes).
   `[[facebook_messages]]` uses the same keys as `[[facebook_comments]]`, and
   `[[instagram_messages]]` the same keys as `[[instagram_comments]]`:

   ```toml
   [[facebook_messages]]
   identifier = "example-page"
   app_id = "9"
   page_id = "100"
   token_env = "EXAMPLE_FACEBOOK_PAGE_TOKEN"
   enabled = true
   schedule = "0 * * * *"

   [[instagram_messages]]
   identifier = "example-account"
   app_id = "9"
   page_id = "100"
   instagram_account_id = "200"
   token_env = "EXAMPLE_FACEBOOK_PAGE_TOKEN"
   ```

3. Restart the daemon, then register and sync:

   ```bash
   msgvault source facebook-messages add example-page
   msgvault source facebook-messages sync example-page
   msgvault source instagram-messages add example-account
   msgvault source instagram-messages sync example-account
   ```

Registration checks the Page, and for Instagram that the Page is linked to the
configured account.

## What is archived

Each conversation becomes a chat with the customer, titled with the customer's
name or Instagram username. Messages the Page or account sent are marked as
yours. The source and message types are `facebook_messages` and
`instagram_messages`. Customer identities are scoped to the app and inbox, so
they never merge with commenters or with people in other sources; use the
[people tools](people.md) to link them.

Meta returns only the **20 most recent messages** of each conversation, so a
sync cannot reach older history. To keep every new message, set up
[Meta webhooks](meta-webhooks.md#inbox-messages): each message, including
the ones the Page or account sends, is then archived as it arrives. Each sync reads every conversation again and
updates messages already archived in place. Each request reads up to 25
conversations, and `--max-requests` limits a run; an unfinished run resumes
from its saved position, and `--full` starts again from the most recent
conversations.

## Limits

- Message text only: attachments, stickers and reactions are not archived.
- A later sync updates the text of a message that is still among the 20 most
  recent, so an edit replaces the archived text. Earlier versions and
  deletions are not kept.
- No sending or replying.
