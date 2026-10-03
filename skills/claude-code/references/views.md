---
title: "msgvault SQL View Reference"
description: "SQL view schemas and column reference for querying msgvault archive analytics."
last_edited: "2026-04-08"
---

# msgvault SQL View Reference

Full column schemas for all views available via `msgvault query`.

## Base Views

These views map directly to Parquet files in `~/.msgvault/analytics/`.

### messages

Hive-partitioned by year. Use `WHERE year = YYYY` for partition pruning.

| Column | Type | Description |
|--------|------|-------------|
| id | BIGINT | Internal message ID |
| source_id | BIGINT | Account/source ID (FK → sources.id) |
| source_message_id | VARCHAR | Gmail message ID |
| conversation_id | BIGINT | Thread ID (FK → conversations.id) |
| subject | VARCHAR | Message subject |
| snippet | VARCHAR | Short preview of message body |
| sent_at | TIMESTAMP | Message send time |
| size_estimate | BIGINT | Estimated message size in bytes |
| has_attachments | BOOLEAN | True if message has attachments |
| attachment_count | INTEGER | Number of attachments (0 if none) |
| sender_id | BIGINT | Participant ID for chat messages (NULL for email) |
| message_type | VARCHAR | Source type (e.g. email, whatsapp, imessage) |
| year | INTEGER | Year of sent_at (partition key) |
| month | INTEGER | Month of sent_at (1–12) |
| deleted_from_source_at | TIMESTAMP | When deleted from Gmail (NULL if still present) |

### participants

| Column | Type | Description |
|--------|------|-------------|
| id | BIGINT | Internal participant ID |
| email_address | VARCHAR | Email address |
| domain | VARCHAR | Domain extracted from email address |
| display_name | VARCHAR | Display name (may be empty) |
| phone_number | VARCHAR | Phone number for chat participants (empty if none) |

### message_recipients

Links messages to participants with a role type.

| Column | Type | Description |
|--------|------|-------------|
| message_id | BIGINT | FK → messages.id |
| participant_id | BIGINT | FK → participants.id |
| recipient_type | VARCHAR | Role: 'from', 'to', 'cc', or 'bcc' |
| display_name | VARCHAR | Display name used in this message (may differ from participants.display_name) |

### labels

| Column | Type | Description |
|--------|------|-------------|
| id | BIGINT | Internal label ID |
| name | VARCHAR | Gmail label name (e.g. 'INBOX', 'SENT', 'Personal') |

### message_labels

| Column | Type | Description |
|--------|------|-------------|
| message_id | BIGINT | FK → messages.id |
| label_id | BIGINT | FK → labels.id |

### attachments

| Column | Type | Description |
|--------|------|-------------|
| message_id | BIGINT | FK → messages.id |
| filename | VARCHAR | Attachment filename |
| size | BIGINT | Attachment size in bytes |

### conversations

| Column | Type | Description |
|--------|------|-------------|
| id | BIGINT | Internal conversation ID |
| source_conversation_id | VARCHAR | Gmail thread ID |
| title | VARCHAR | Conversation title (empty string if none) |
| conversation_type | VARCHAR | Thread type (e.g. email, direct_chat, group_chat) |

### sources

| Column | Type | Description |
|--------|------|-------------|
| id | BIGINT | Internal source ID |
| source_type | VARCHAR | Account type (e.g. gmail, imap, mbox, apple-mail) |

(Additional columns such as email address are present but not normalised in the view.)

---

## Convenience Views

Pre-joined and aggregated views for common query patterns.

### v_messages

Messages with sender fully resolved and labels as a JSON array. Use this instead of `messages` whenever you need sender identity or label data.

| Column | Type | Description |
|--------|------|-------------|
| id | BIGINT | Internal message ID |
| source_id | BIGINT | Account/source ID |
| source_message_id | VARCHAR | Gmail message ID |
| conversation_id | BIGINT | Thread ID |
| subject | VARCHAR | Message subject |
| snippet | VARCHAR | Short preview |
| sent_at | TIMESTAMP | Send time |
| size_estimate | BIGINT | Estimated size in bytes |
| has_attachments | BOOLEAN | True if message has attachments |
| attachment_count | INTEGER | Number of attachments |
| message_type | VARCHAR | Source type (e.g. email, whatsapp, imessage) |
| year | INTEGER | Year (partition key) |
| month | INTEGER | Month (1–12) |
| from_email | VARCHAR | Sender email address |
| from_name | VARCHAR | Sender display name |
| from_domain | VARCHAR | Sender domain |
| from_phone | VARCHAR | Sender phone (chat messages; empty for email) |
| labels | VARCHAR | JSON array of label names, e.g. `["INBOX","IMPORTANT"]` |
| deleted_from_source_at | TIMESTAMP | When deleted from Gmail (NULL if present) |

**Label filtering:** Use `json_array_contains(labels, 'INBOX')` or join through `message_labels` + `labels`.

**Sender resolution:** Resolved via `message_recipients` for email; falls back to `messages.sender_id` for chat.

### v_senders

Per-sender aggregates across all messages.

| Column | Type | Description |
|--------|------|-------------|
| from_email | VARCHAR | Sender email address |
| from_name | VARCHAR | Best available display name |
| from_domain | VARCHAR | Sender domain |
| message_count | BIGINT | Total messages received from this sender |
| total_size | BIGINT | Sum of size_estimate across all messages |
| attachment_size | BIGINT | Sum of attachment sizes (0 if none) |
| attachment_count | BIGINT | Total attachment count (0 if none) |
| first_message_at | TIMESTAMP | Earliest message from this sender |
| last_message_at | TIMESTAMP | Most recent message from this sender |

### v_domains

Per-domain aggregates.

| Column | Type | Description |
|--------|------|-------------|
| domain | VARCHAR | Sender domain |
| message_count | BIGINT | Total messages from this domain |
| total_size | BIGINT | Sum of size_estimate |
| sender_count | BIGINT | Distinct sender email addresses from this domain |

### v_labels

Per-label aggregates.

| Column | Type | Description |
|--------|------|-------------|
| name | VARCHAR | Gmail label name |
| message_count | BIGINT | Number of messages with this label |
| total_size | BIGINT | Sum of size_estimate for labelled messages |

### v_threads

Per-conversation aggregates with participant list.

| Column | Type | Description |
|--------|------|-------------|
| conversation_id | BIGINT | Internal conversation ID |
| source_conversation_id | VARCHAR | Gmail thread ID |
| conversation_title | VARCHAR | Conversation title (may be empty) |
| conversation_type | VARCHAR | Thread type (e.g. email, direct_chat, group_chat) |
| message_count | BIGINT | Number of messages in this thread |
| first_message_at | TIMESTAMP | Earliest message in thread |
| last_message_at | TIMESTAMP | Most recent message in thread |
| participant_emails | VARCHAR | JSON array of distinct participant email addresses |
