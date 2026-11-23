### Bug fixes

- Beeper sync now moves past attachments that the source reports no longer exist.
- A failed Beeper attachment download no longer holds back source freshness; the error is still reported and the file stays pending for `msgvault backfill-beeper-media`.
