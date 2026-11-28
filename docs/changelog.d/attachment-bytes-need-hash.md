### Bug fixes

- Backups now include Slack and Discord attachments that older releases saved without a content hash. These were duplicate files whose original copy had since been removed, so no backup captured them. Upgrading reads each file's hash from its stored file name.
- An attachment marked as downloaded but saved without a content hash now shows as a failed download, so a later sync can fetch it again. Backups never captured these attachments.
