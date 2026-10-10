package inboxcontrol

// Candidate carries committed provider metadata, never message bodies. Available
// means the required markers are present; execution still needs live preflight.
type Candidate struct {
	State     State  `json:"state"`
	Title     string `json:"title,omitempty"`
	Snippet   string `json:"snippet,omitempty"`
	Available bool   `json:"available"`
	// ContextMessageID selects the newest active archived message in a chat.
	// Zero means no archived selector is available; mail uses Target.ItemID.
	ContextMessageID int64 `json:"context_message_id,omitempty"`
}

// CandidatePage is a bounded source-scoped archive snapshot. Unavailable reports
// missing provider evidence rather than interpreting unknown membership as empty.
type CandidatePage struct {
	ProviderIngestion *ProviderIngestion `json:"provider_ingestion,omitempty"`
	Source            SourceIdentity     `json:"source"`
	Scope             Scope              `json:"scope"`
	ArchiveRevision   string             `json:"archive_revision"`
	Candidates        []Candidate        `json:"candidates"`
	NextCursor        string             `json:"next_cursor,omitempty"`
	Unavailable       bool               `json:"unavailable"`
}

// ProviderIngestion describes known ingestion gaps separately from committed
// archive state. A successful sync or analytics publication does not establish
// that every provider item has been ingested.
type ProviderIngestion struct {
	Status string `json:"status" enum:"unknown,partial"`
	Reason string `json:"reason"`
}

// UnknownProviderIngestion is the conservative default when completeness has
// not been independently verified.
func UnknownProviderIngestion() *ProviderIngestion {
	return &ProviderIngestion{Status: "unknown", Reason: "provider_completeness_unverified"}
}
