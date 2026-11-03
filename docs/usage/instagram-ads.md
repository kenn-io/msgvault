---
last_edited: 2026-11-03
title: Instagram Ad Comments
description: Discover verified Instagram ad media through an explicitly selected ad account.
---

Find comments on Instagram ad media that the ordinary account media edge does
not return. This unreleased option adds creative discovery to
[Instagram comment sync](instagram-comments.md); it uses the same archive,
conversations, search, and person links. It does not send replies.

## Select the ad read boundary

Start with a registered linked professional Instagram account. Add an explicit
ad account ID and the name of its credential variable to that existing profile:

```toml
[[instagram_comments]]
identifier = "example-account"
app_id = "9"
page_id = "100"
instagram_account_id = "200"
token_env = "EXAMPLE_INSTAGRAM_PAGE_TOKEN"
ad_account_id = "700"
ad_token_env = "EXAMPLE_META_AD_TOKEN"
```

Both ad settings are required together. Use the decimal account ID without
`act_`; msgvault adds that prefix for the fixed ad-creative endpoint. Keep
credential values only in the named variables on the daemon host. The ad
credential reads the selected account's creatives; the Page-context credential
reads the canonical Instagram media, comments, and replies. There is no
implicit credential fallback or token acquisition. Actual Meta access grants
and object availability determine which reads succeed.

Restart the daemon after changing its environment or profile. When enabling,
disabling, or changing ad discovery on an existing source, explicitly restart
its progress:

```bash
msgvault source instagram-comments sync example-account --full --max-requests 1000
```

`--full` resets cursors under the same budget, preserving and updating archived
rows. Later runs use the same command without `--full`. The app/Page/Instagram
source identity and scheduler job remain unchanged; the checkpoint also binds
the selected ad account. Incompatible progress is refused rather than silently
upgraded or retargeted.

## Understand coverage

Each creative page and canonical media lookup counts toward the shared request
budget, along with the two Page/link verification reads. Organic media and ad
creatives retain separate bounded progress and take turns across runs. A denied
ad edge does not prevent later organic updates. New cycles revisit old media;
there is no creation-date cutoff.

Creative IDs are discovery references, never comment roots. Only the returned
effective Instagram media ID is resolved. The response ID must match, and its
nonempty typed owner ID must equal the configured linked Instagram account.
Foreign or unverified objects are refused before actor or message writes.
Other resolvable candidates can still import. The first refusal is reported,
and a durable rescan obligation prevents an earlier refusal from becoming
successful coverage after a budget pause. HTTP failures retain the exact
candidate or cursor and original error; they never imply automatic expiry,
reset, or field removal.

Organic and ad discovery update one canonical media conversation. Metadata
keeps one first verified ad-account/creative association, inherited by comments
and replies and preserved during ordinary replay. This is bounded discovery
provenance, not an exhaustive creative history or proof that a comment came
from a paid placement. Enriched records use metadata format version 2 so older
importers refuse them rather than dropping that association on replay.

## Limits

Missing effective media, missing ownership, partnership, dynamic, and other
unresolvable variants keep ad coverage partial; configured permissions alone
do not establish complete coverage. Returned Facebook Page ad posts remain
supported by [Page comment sync](facebook-comments.md). This option does not
import arbitrary Facebook stories from creative IDs: their additional Page
ownership contract remains unverified. Public replies are separate work.
