# Slack Limit-One Sweep Progress Design

## Problem

A Slack reply sweep overlaps its persisted floor by ten minutes so it can
recover replies that were indexed late. When the floor is at or shortly after
midnight in the user's timezone, that overlap begins on the previous calendar
day. The sweep currently charges that already-certified day against `--limit`
before checking whether its next boundary can advance the floor. With
`--limit 1`, every run spends its entire budget on the same day and the sweep
never reaches new coverage.

The same state arises during ordinary daytime operation when a limited backlog
run advances its floor to an exact midnight boundary. Repeated limited runs are
documented to converge, so this is a production correctness bug rather than a
test-only timing problem.

## Design

Keep searching overlap days so late-indexed replies remain recoverable, but do
not charge a day against the limited-run budget when that day's next boundary
is at or below the persisted floor. Such a day can only re-certify coverage the
workspace already owns; charging it cannot represent forward sweep work.

Once the walk reaches a day whose next boundary is above the floor, retain the
existing check-before-charge behavior. Search, durable debt recording,
checkpointing, truncation handling, and canonical thread fetch accounting stay
unchanged.

This is narrower than removing the overlap or exempting all first days. It
preserves the index-lag safety window and exempts only intervals that cannot
advance certification.

## Regression Coverage

Adapt the three deterministic scenarios from the issue reporter's branch into
a self-contained regression test file:

1. The initial watermark lands three minutes after midnight.
2. A limited run naturally advances the watermark to exactly midnight.
3. An ordinary midday watermark falls a day behind and limited catch-up lands
   on a midnight boundary.

Each scenario will add a late reply to an old thread, run repeated
`--limit 1` imports through the real importer and fake Slack server, and assert
that the reply is archived before the seven-day canonical audit can mask sweep
behavior. Fixed clocks and fixture timestamps will be local to these tests so
the patch does not refactor unrelated package tests.

## Error Handling and Compatibility

No persisted state or command-line interface changes. Existing discovery,
fetch, truncation, and checkpoint errors retain their behavior. Unlimited runs
are behaviorally unchanged apart from avoiding a meaningless budget increment,
and limited runs still count every day capable of advancing the floor.

## Validation

The regression tests must fail against the current implementation with the
late replies absent, then pass after the budget exemption. The full Slack
package, formatting, vet, lint, and repository test suite must remain clean.
