---
last_edited: "2026-11-08"
title: GitHub
description: Archive the GitHub issues, pull requests, discussions, and releases you take part in, next to your mail and chats.
---

Keep the GitHub conversations you take part in searchable next to your mail
and chats: issues and pull requests with every comment and review, discussions
with their replies, and the releases of repositories you watch. msgvault reads
them from GitHub's API, so you need no notification email, and it keeps them
after GitHub drops your notifications.

## Add an account

GitHub's notifications API accepts only classic personal access tokens.
Create one at [github.com/settings/tokens](https://github.com/settings/tokens)
with the `notifications` scope. Add the `repo` scope to also archive private
repositories. Then add the account:

```bash
msgvault add-github
msgvault add-github --token-file ~/github-token.txt
MSGVAULT_GITHUB_TOKEN="ghp_..." msgvault add-github
```

`add-github` checks the token and stores it in msgvault's tokens directory.
Each account becomes a source named `GitHub <login>`, and the account's own
posts count as sent by you.

## Keep it current

Sync every added account on a schedule with the `[github]` block in
`config.toml`:

```toml
[github]
enabled = true
schedule = "*/15 * * * *"
exclude_owners = ["my-employer"]
```

Run `msgvault sync-github` to sync now, or `msgvault sync-github <login>` for
one account. Once the schedule is set, **Sync now** on a GitHub source in the
Sources page syncs every added account.

Each sync finds the threads that changed since the last one and archives every
post in them:

- **Notifications.** Every thread GitHub notifies you about, read or not:
  threads you are subscribed to, review requests, mentions, and releases of
  repositories you watch for releases. An unchanged notification list costs
  nothing against your rate limit.
- **Involvement search.** Every issue, pull request, and discussion you
  authored, commented on, were assigned, or were mentioned in. The first sync
  searches your whole history, so threads older than your notifications are
  archived too.

A changed thread is fetched whole, and posts the archive holds unchanged are
not written again. The newest threads are archived first.

## Choose whose repositories are archived

`include_owners` and `exclude_owners` name organizations and users, the owners
of the repositories. Threads in an excluded owner's repositories are dropped
from the notification list, and left out of the search itself, before any of
their content is fetched:

| Key | Archives |
|---|---|
| neither | Every repository |
| `include_owners = ["kenn-io", "octocat"]` | Only these owners' repositories |
| `exclude_owners = ["my-employer"]` | Every repository except this owner's |

After you change either list, the next sync searches your history again, so a
newly allowed owner's older threads are archived too.

## Rate limits

Your GitHub tools share one hourly rate limit per account. A sync leaves a
fifth of each limit for them. A first sync with a long history can reach that
reserve; it then stops, keeps the threads it found, and the next sync
continues. `sync-github` prints how many threads wait and when the limit
resets.

## What gets archived

Each issue, pull request, discussion, and release is a conversation, named as
GitHub's notification emails name it, such as
`[octocat/hello-world] Fix the greeting (PR #12)`. Every post is a message of
type `github`:

```bash
msgvault search "flaky test" --message-type github
```

| Post | Message |
|---|---|
| Issue, pull request, or discussion | The opening post: its title is the subject and its description the body |
| Comment, discussion comment, or reply | A message with the comment as its body |
| Pull request review | Its verdict, such as "Approved these changes.", then its text. A comment-only review without text adds no message; its review comments do |
| Review comment | The file and line it comments on, then the comment |
| Release | The release notes, with the release name as the subject |

- Bodies are kept as written in GitHub Markdown, and each ends with the post's
  link.
- `metadata.kind` names the kind of post, such as `review_comment`, beside the
  repository, the number, and kind-specific fields: a review's state, a review
  comment's path and line, a discussion's category, and whether a comment is
  the discussion's answer.
- The conversation's metadata keeps the thread's link and the reason of its
  latest notification, such as `review_requested` or `mention`.
- An edited post replaces its archived copy at the next sync that finds the
  thread changed. A post deleted on GitHub stays archived.
- Each GitHub account is one person in msgvault, named by its login and keyed
  by its account ID, so a renamed account stays the same person. A deleted
  account appears as `ghost`.

## Remove an account

```bash
msgvault remove-account --type github <account-id>
```

`msgvault list-accounts` shows each GitHub source's account ID. Removing the
source deletes its archived messages and its stored token, and cannot be
undone.

## Limitations

- GitHub.com only; GitHub Enterprise Server is not supported.
- Images and files in posts are linked, not downloaded.
- Reactions, labels, and events such as merges and closes are not archived.
- Notifications about CI runs, security alerts, and commits are skipped: they
  have no conversation to archive.
- A review comment keeps its file and line, not the surrounding diff.
