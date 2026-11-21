---
last_edited: "2026-11-08"
title: Code Forges
description: Archive the GitHub, GitLab, Gitea and Forgejo issues, pull and merge requests, discussions and releases you take part in, next to your mail and chats.
---

Keep the code-forge conversations you take part in searchable next to your mail
and chats: issues and pull requests with every comment and review, discussions
with their replies, and the releases of repositories you watch. msgvault reads
them from the forge's API, so you need no notification email, and it keeps
them after the forge drops your notifications.

msgvault archives [GitHub](#github), [GitLab](#gitlab), and
[Gitea and Forgejo](#gitea-and-forgejo), such as Codeberg.

## GitHub

### Add an account

GitHub's notifications API accepts only classic personal access tokens. Create
one at [github.com/settings/tokens](https://github.com/settings/tokens):

| Scope | Gives msgvault |
|---|---|
| `notifications` and `public_repo` | Your notifications, and discussions in public repositories: required |
| `repo` | Everything both give, and private repositories' threads |
| `read:user` | Other accounts' public emails, which join a GitHub account to the same person in your mail |

Then add the account:

```bash
msgvault add-github
msgvault add-github --token-file ~/github-token.txt
MSGVAULT_GITHUB_TOKEN="ghp_..." msgvault add-github
```

`add-github` checks the token and stores it in msgvault's tokens directory.
The account becomes a source named `GitHub <login>`, and its own posts count as
sent by you.

### What a sync finds

Each sync finds the threads that changed since the last one and archives every
post in them:

- **Notifications.** Every thread GitHub notifies you about, read or not:
  threads you're subscribed to, review requests, mentions, and releases of
  repositories you watch for releases. An unchanged notification list costs
  nothing against your rate limit.
- **Searches.** Every issue, pull request and discussion you authored,
  commented on, were assigned, or were mentioned in, and every pull request
  you reviewed or were asked to review. The first sync searches your whole
  history, so threads older than your notifications are archived too.

Notifications about CI runs, security alerts and commits are skipped: they have
no conversation to archive. A discussion's notification names only its title, so
when two discussions updated in the last month share it, both are archived.

## GitLab

### Add an account

Create a legacy personal access token with the `read_api` and `read_user`
scopes: select your avatar, **Edit profile**, **Access > Personal access
tokens**, then **Generate token > Legacy token**. Then add the account:

```bash
msgvault add-gitlab
msgvault add-gitlab --url https://gitlab.example.com
MSGVAULT_GITLAB_TOKEN="glpat-..." msgvault add-gitlab
```

Without `--url` the account is on gitlab.com. A self-managed instance is named
by its web address, path included. The account becomes a source named
`GitLab <login>`, followed by the instance when it isn't gitlab.com.

### What a sync finds

GitLab has no notifications API and no search for everything you take part in,
so each sync reads several lists:

- **To-dos.** Every thread GitLab gave you a to-do for, pending or done:
  mentions, assignments and review requests.
- **Your events.** Every thread you opened or approved, and every comment you
  wrote, including replies in discussions. GitLab keeps three years of events.
- **Your lists.** Every issue and merge request you authored or are assigned,
  and every merge request you review.
- **Archived threads.** Each sync checks the threads it archived before, so
  other people's later replies and edits arrive even when nothing above lists
  them again.

## Gitea and Forgejo

### Add an account

Create an access token under **Settings > Applications** on the server, with
read access to `notification`, `issue`, `repository` and `user`. Then add the
account by the server's web address:

```bash
msgvault add-gitea --url https://codeberg.org
msgvault add-gitea --url https://git.example.com --token-file ~/gitea-token.txt
MSGVAULT_GITEA_TOKEN="..." msgvault add-gitea --url https://codeberg.org
```

`add-gitea` checks that the token may read each of them. The account becomes a
source named `Gitea <login> (<server>)`. Forgejo serves Gitea's API, so a
Forgejo account is added the same way.

### What a sync finds

- **Notifications.** Every issue and pull request the server notifies you
  about, read or not: those you watch or take part in, are assigned or asked
  to review, or are mentioned in.
- **Searches.** Every issue and pull request you created, were assigned, were
  mentioned in, were asked to review, or reviewed.
- **Your activity.** Every issue and pull request you commented on, from your
  activity feed, which the server keeps unless its admin turns on deleting old
  actions.
- **Archived threads.** Each sync also checks up to 50 of the threads it
  archived before, in turn. It checks each thread's update time, then recent
  edits to plain thread comments if the thread's update time has not changed.
  New replies, reviews, code comments and edits to the opening post or plain
  thread comments arrive even when none of the lists above shows the thread
  again.

These checks cannot detect edits to existing code comments. Those edits arrive
when the pull request is fetched again.

## Keep it current

Sync every added account on a schedule with the forge's block in
`config.toml`:

```toml
[github]
enabled = true
schedule = "*/15 * * * *"
exclude_owners = ["my-employer"]

[gitlab]
enabled = true
schedule = "*/15 * * * *"

[gitea]
enabled = true
schedule = "*/15 * * * *"
```

Run `msgvault sync-github`, `sync-gitlab` or `sync-gitea` to sync now, or name
one account by its login. Once a forge's schedule is set, **Sync now** on one of its
sources in the Sources page syncs every account of that forge.

The newest threads are archived first. A changed thread is fetched whole, and
posts the archive holds unchanged are not written again. `--full` finds and
fetches every thread again.

## Choose whose repositories are archived

`include_owners` and `exclude_owners` name the organizations and users that own
repositories; on GitLab, a project's owner is its top-level group or user, so
excluding a group excludes its subgroups too:

| Setting | Archives |
|---|---|
| neither | Every repository |
| `include_owners = ["kenn-io", "octocat"]` | Only these owners' repositories |
| `exclude_owners = ["my-employer"]` | Every repository except this owner's |

An excluded owner's threads are never fetched or stored. On GitHub they never
reach msgvault at all: the search leaves them out, and notifications carry
only titles, which msgvault drops. Each sync also looks up a repository's
current owner before fetching any of its threads, so a repository moved to an
excluded owner after it was found is dropped too. GitLab's lists of to-dos,
events, issues and merge requests, and Gitea's notifications and activity, hold
every owner's items, and msgvault drops an excluded owner's as it reads each
page. Gitea's search takes one owner per request, so `include_owners` is
searched owner by owner. After you change either list, the
next sync searches your history again, so a newly allowed owner's older threads
are archived.

## Rate limits

Your tools share each forge's rate limit. On GitHub a sync pauses once GitHub
reports that a fifth or less of an hourly limit is left, leaving the rest to
them; a first sync with a long history can get there. On GitLab, a sync waits
out a refusal that resets within a minute, and pauses at one that resets later.
Gitea and Forgejo limit no requests themselves; a sync treats a refusal from a
proxy in front of a hosted server the same way. A paused sync keeps its place,
partway through a long thread if need be, and the next sync continues. Each
sync command prints how many threads wait and when the limit resets. A paused
sync is not a failed one: the Sources page shows it as paused.

## What gets archived

Each issue, pull or merge request, discussion and release is a conversation,
named as the forge's notification emails name it, such as
`[octocat/hello-world] Fix the greeting (PR #12)` or
`[group/project] Fix the greeting (!12)`. Every post is a message of its
forge's type, `github`, `gitlab` or `gitea`:

```bash
msgvault search "flaky test" --message-type gitlab
```

| Post | Message |
|---|---|
| Issue, pull or merge request, or discussion | The opening post: its title is the subject and its description the body |
| Comment, discussion comment or reply | A message with the comment as its body |
| Pull request review | Its verdict, such as "Approved these changes.", then its text. A comment-only review without text adds no message; its review comments do |
| Review comment | The file and line it comments on, then the comment |
| Release | The release notes, with the release name as the subject |
| GitLab note, thread reply or diff note | A message with the note as its body; a diff note starts with its file and line |
| GitLab approval | "Approved this merge request." |

- Bodies are kept as written, in Markdown, and each ends with the post's link.
  The forge's own JSON for each post is kept as its original.
- `metadata.kind` names the kind of post, such as `review_comment`, beside the
  repository, the number, and kind-specific fields: a review's state, a review
  comment's path and line, a discussion's category, whether a comment is the
  discussion's answer, a GitLab note's discussion and whether it is resolved,
  and whether a Gitea review comment's conversation is resolved.
- The conversation's metadata keeps the thread's link and the reason of its
  latest notification or to-do, such as `review_requested` or `mention`.
- An edited post replaces its archived copy at the next sync that finds the
  thread changed. A post deleted on the forge stays archived.

## People

Each forge account is one person in msgvault, keyed by its account ID, so a
renamed account stays the same person; it is named by its login. With a token
that may read public emails, a GitHub or GitLab account that shows one joins
the person who has that address in your mail. Bots never join by email, and
Gitea shows most accounts' emails as placeholders, so a Gitea account never
does. A deleted account appears as `ghost`, and an account a migration brought
over from another forge is a name of that source only.

## Remove an account

```bash
msgvault remove-account --type github <identifier>
msgvault remove-account --type gitlab <identifier>
msgvault remove-account --type gitea <identifier>
```

`msgvault list-accounts` shows each account's identifier, such as
`github.com:user:583231`, `gitlab.com:user:1234` or `codeberg.org:user:5678`. Removing the source deletes its archived messages
and its stored token, and cannot be undone.

## Limitations

- GitHub.com only; GitHub Enterprise Server is not supported.
- GitLab epics, snippets, commit comments, and releases are not archived.
- Gitea and Forgejo releases and commit comments are not archived.
- Images and files in posts are linked, not downloaded.
- Reactions, labels, and events such as merges and closes are not archived.
- A review comment keeps its file and line, not the surrounding diff.
