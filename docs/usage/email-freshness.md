# Email freshness

The daemon can poll existing Gmail and IMAP accounts between scheduled cron syncs. Set `poll_interval` to a whole number of seconds from `15s` through `120s` in an enabled `[[accounts]]` entry. Keep a valid five-field `schedule` as a backstop.

```toml
[[accounts]]
email = "user@example.com"
schedule = "0 * * * *"
enabled = true
poll_interval = "30s"
```

The account identifier or display name must match one active Gmail or IMAP source. Use the exact identifier shown by `msgvault list-accounts` if a name is ambiguous. IMAP polling covers all folders and requires an account without saved folder exclusions.

The daemon starts a catch-up when it starts. Polls wait behind other work, and provider limits can delay acquisition. Search and analytics updates run in the background after acquisition, so new messages may take longer to appear there. An incomplete pass keeps its saved progress and retries on a later trigger.
