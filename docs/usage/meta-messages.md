---
last_edited: "2026-11-13"
title: Facebook and Instagram Messages
description: Archive a Facebook Page's Messenger inbox or a linked Instagram account's direct messages as searchable chats.
---

Search customer conversations from your Facebook Page on Messenger or your
professional Instagram account in Instagram direct messages alongside your
other chats. msgvault reads these inboxes with the Page token and does not send
messages.

This integration archives a business inbox. To import your personal Messenger
history, use [`import-messenger`](text-messages.md#import-messenger).

## Connect an inbox

1. Get a Page access token for your Page with the messaging permissions granted
   to your Meta app, and store it in a named environment variable on the
   **daemon host**.
2. Add a profile to your [configuration](../configuration.md#meta-inboxes).
   `[[facebook_messages]]` uses the same keys as `[[facebook_comments]]`, and
   `[[instagram_messages]]` uses the same keys as `[[instagram_comments]]`:

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

3. Restart the daemon.
4. Register and sync the inbox:

   ```bash
   msgvault source facebook-messages add example-page
   msgvault source facebook-messages sync example-page
   msgvault source instagram-messages add example-account
   msgvault source instagram-messages sync example-account
   ```

During registration, msgvault checks the Page and, for Instagram, verifies that
the Page is linked to the configured account.

## What is archived

Each one-to-one conversation becomes a chat with the customer, titled with the
customer's name or Instagram username. Messages sent by the Page or account are
marked as yours, with `facebook_messages` and `instagram_messages` as the
source and message types. Customer identities are scoped to the app and inbox;
to link them to other people, use the [people tools](people.md).

To archive new messages as they arrive, set up
[Meta webhooks](meta-webhooks.md#inbox-messages). With webhooks active,
msgvault archives each new message that has text, including messages sent by
the Page or account. If a webhook change is skipped or never delivered, the
next sync archives that message only if it is still among the 20 most recent in
the conversation.

Each sync reads every conversation again and updates archived messages in
place. One request reads up to 25 conversations, and `--max-requests` caps a
run; an unfinished run resumes from its saved position next time. If a sync
fails with `continuation request failed` while your token is valid, run it
again with `--full`.

## Limits

- Meta returns only the **20 most recent messages** in each conversation.
- msgvault archives message text only; attachments, stickers, and reactions are
  not archived.
- Only one-to-one conversations between the Page or account and one other
  person are archived. Sync skips group chats and counts them in its output.
- Edits replace the archived text. Earlier versions and deletions are not kept,
  and webhooks ignore edit and deletion events.
