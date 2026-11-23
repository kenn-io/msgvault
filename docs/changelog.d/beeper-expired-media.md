### Bug fixes

- Beeper sync now moves past attachments that the source reports no longer exist.
- A failed Beeper attachment download no longer holds back source freshness, as long as its pending marker was stored, nothing else in the run failed, and the run was not stopped early. The error is still reported and the file stays pending for `msgvault backfill-beeper-media`.
