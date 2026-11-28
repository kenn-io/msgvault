### Improvements

- Slack and Gmail syncs write each fetched page or batch of messages in one database transaction, so initial imports write several times less to disk. If a Slack page fails partway, none of its messages are kept and the next sync fetches the page again. A Gmail message that fails still leaves nothing behind, and the batch's other messages are kept.
