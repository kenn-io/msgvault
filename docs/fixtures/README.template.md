# Controlled web UI fixture

This orphan-branch artifact contains a compact MBOX selected from the May 7,
2015 CMU CALO Enron release for documentation evidence. It is not the full
corpus and is not a substitute for reviewing the source release’s terms or
attribution guidance.

Source: https://www.cs.cmu.edu/~enron/

The selection algorithm, deterministic obvious-sensitive-content pre-filter,
source checksum, exclusions, fixture checksum, and message-by-message
sensitive-content review are recorded in `manifest.json`. The pre-filter is a
conservative first pass, not a replacement for reading every selected message.
The manifest also records the selected sender used as the deterministic import
owner; MIME attachments are excluded from the published text-only fixture.
Authentic names and message text are intentional in this controlled fixture;
they must not be copied into ordinary tests or examples. Any refresh must
select again, read every selected message, publish a new append-only commit,
and update the main-branch lock only after that commit is reachable from the
remote.

To refresh locally, download the pinned source release into a private 0700
directory, run `select_enron_fixture.py`, read the complete report produced by
`review_enron_fixture.py`, record any exclusions and repeat the full review,
then run `publish-fixture-branch.sh --push`. Never force-push this branch.
