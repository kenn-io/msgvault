---
last_edited: "2026-11-13"
title: Facebook and Instagram Messages
description: Archive a Facebook Page's Messenger inbox or a linked Instagram account's direct messages as searchable chats.
---

Search the conversations customers have with your Facebook Page on Messenger,
or with your professional Instagram account in Instagram direct messages,
beside your other chats. msgvault reads these inboxes with the Page token; it
does not send messages.

This archives a business inbox. For your personal Messenger history, use
[`import-messenger`](text-messages.md#import-messenger).

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

Each one-to-one conversation becomes a chat with the customer, titled with the
customer's name or Instagram username. Messages the Page or account sent are
marked as yours. The source and message types are `facebook_messages` and
`instagram_messages`. Customer identities are scoped to the app and inbox; use
the [people tools](people.md) to link them to other people.

To archive new messages as they arrive, set up
[Meta webhooks](meta-webhooks.md#inbox-messages). Each new message with text
is then archived, including the ones the Page or account sends. A message
whose webhook change was skipped or never delivered is archived only if it is
still among the 20 most recent when the next sync runs.

Each sync reads every conversation again and updates archived messages in
place. Each request reads up to 25 conversations, and `--max-requests` limits
a run; an unfinished run resumes from its saved position. If a sync fails with
`continuation request failed` and the token is valid, run it again with
`--full`.

## Limits

- Meta returns only the **20 most recent messages** of each conversation.
- Message text only: attachments, stickers and reactions are not archived.
- Only conversations between the Page or account and one other person are
  archived. A sync skips group chats and counts them in its output.
- Edits replace the archived text; earlier versions and deletions are not
  kept, and webhooks ignore edit and deletion events.
