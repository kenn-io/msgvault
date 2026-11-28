### Improvements

- Discord sync writes each fetched page of messages in one database transaction, so large imports run faster and put less load on PostgreSQL. If a page fails partway, none of its messages are kept and the next sync fetches the page again.
