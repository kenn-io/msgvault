package emailtags

// ArchiveChange requires an explicit desired state, including false for unarchive.
type ArchiveChange struct {
	Archived *bool `json:"archived" nullable:"false"`
	DryRun   bool  `json:"dry_run,omitempty"`
}

// ArchiveResult describes the verified provider state or the dry-run projection.
type ArchiveResult struct {
	MessageID int64 `json:"message_id"`
	SourceID  int64 `json:"source_id"`
	Archived  bool  `json:"archived"`
	Changed   bool  `json:"changed"`
	DryRun    bool  `json:"dry_run"`
	Verified  bool  `json:"verified"`
}
