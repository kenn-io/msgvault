package whoknows

import "time"

type WhoKnowsEvidence struct {
	MessageID           int64      `json:"message_id"`
	SourceMessageID     string     `json:"source_message_id"`
	ConversationID      int64      `json:"conversation_id"`
	MessageType         string     `json:"message_type"`
	Role                string     `json:"role"`
	SentAt              *time.Time `json:"sent_at"`
	Relevance           float64    `json:"relevance"`
	VectorScore         *float64   `json:"vector_score"`
	Recency             float64    `json:"recency"`
	Weight              float64    `json:"weight"`
	Contribution        float64    `json:"contribution"`
	Subject             string     `json:"subject"`
	Snippet             string     `json:"snippet"`
	Flags               []string   `json:"flags"`
	SenderAuthenticated bool       `json:"sender_authenticated"`
	Excerpt             *Excerpt   `json:"excerpt"`
	ExcerptError        bool       `json:"excerpt_error"`
	Channel             string     `json:"-"`
}
type WhoKnowsExcerpt struct {
	Snippet    string  `json:"snippet"`
	CharOffset *int    `json:"char_offset"`
	Line       *int    `json:"line"`
	Score      float64 `json:"score"`
}
type WhoKnowsBreakdown struct {
	Evidence            float64    `json:"evidence"`
	Depth               float64    `json:"depth"`
	RawAuthored         float64    `json:"raw_authored"`
	RawReceived         float64    `json:"raw_received"`
	Breadth             float64    `json:"breadth"`
	Declared            float64    `json:"declared"`
	Affinity            float64    `json:"affinity"`
	ProfileSearch       string     `json:"profile_search"`
	ProfileScore        *float64   `json:"profile_score"`
	ProfileSim          float64    `json:"profile_sim"`
	OrgMatch            float64    `json:"org_match"`
	AuthoredMessages    int        `json:"authored_messages"`
	ReceivedMessages    int        `json:"received_messages"`
	EvidenceTotal       int        `json:"evidence_total"`
	ShownContribution   float64    `json:"shown_contribution"`
	OmittedContribution float64    `json:"omitted_contribution"`
	ThreadsAuthored     int        `json:"threads_authored"`
	ChannelsAuthored    []string   `json:"channels_authored"`
	LatestAuthoredAt    *time.Time `json:"latest_authored_at"`
	AskMeAboutMatches   []string   `json:"ask_me_about_matches"`
	OrganizationMatches []string   `json:"organization_matches"`
}
type WhoKnowsOrganization struct {
	OrganizationID int64  `json:"organization_id"`
	Name           string `json:"name"`
	Title          string `json:"title"`
	NameNormalized string `json:"-"`
	PrimaryDomain  string `json:"-"`
}
type WhoKnowsTieValue struct {
	Temperature   *int       `json:"temperature"`
	LastContactAt *time.Time `json:"last_contact_at"`
}
type Candidate struct {
	PersonID, ParticipantID             int64
	VCardUID, DisplayName, EmailAddress string
	Evidence                            []Evidence
	AskMeAboutMatches                   []string
	ProfileScore                        *float64
	ProfileSearch                       string
	Organizations                       []Organization
	OneWay                              bool
	LastContactAt                       *time.Time
	Temperature                         *int
}
type WhoKnowsRanked struct {
	Rank          int            `json:"rank"`
	PersonID      int64          `json:"person_id,omitzero"`
	ParticipantID int64          `json:"participant_id,omitzero"`
	VCardUID      string         `json:"vcard_uid,omitempty"`
	DisplayName   string         `json:"display_name"`
	EmailAddress  string         `json:"email_address,omitempty"`
	Score         float64        `json:"score"`
	Flags         []string       `json:"flags"`
	Breakdown     Breakdown      `json:"breakdown"`
	Evidence      []Evidence     `json:"evidence"`
	Organizations []Organization `json:"organizations"`
	Tiebreak      TieValue       `json:"tiebreak"`
	PromoteHint   string         `json:"promote_hint,omitempty"`
}
type WhoKnowsOrganizationGroup struct {
	OrganizationID int64   `json:"organization_id"`
	Name           string  `json:"name"`
	Score          float64 `json:"score"`
	People         []int64 `json:"people"`
}
type WhoKnowsExcluded struct {
	MessageTypes       int `json:"message_types"`
	OwnerMessages      int `json:"owner_messages"`
	OwnerPeople        int `json:"owner_people"`
	AutomatedSenders   int `json:"automated_senders"`
	LargeRooms         int `json:"large_rooms"`
	UnresolvedMessages int `json:"unresolved_messages"`
}
type WhoKnowsGeneration struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Model       string `json:"model"`
	Fingerprint string `json:"fingerprint"`
	Dimension   int    `json:"dimension"`
}
type WhoKnowsCoverage struct {
	Generation   Generation      `json:"generation"`
	Scope        Scope           `json:"scope"`
	Messages     MessageCoverage `json:"messages"`
	PeopleIndex  PeopleCoverage  `json:"people_index"`
	ContactState ContactCoverage `json:"contact_state"`
	Stale        bool            `json:"stale"`
	StaleReasons []string        `json:"stale_reasons"`
}
type WhoKnowsScope struct {
	MessageTypes []string `json:"message_types"`
	Accounts     []int64  `json:"accounts"`
}
type WhoKnowsMessageCoverage struct {
	Eligible          int64     `json:"eligible"`
	Current           int64     `json:"current"`
	Pending           int64     `json:"pending"`
	CoverageSampledAt time.Time `json:"coverage_sampled_at"`
}
type WhoKnowsPeopleCoverage struct {
	Enabled   bool `json:"enabled"`
	Consented bool `json:"consented"`
	Complete  bool `json:"complete"`
}
type WhoKnowsContactCoverage struct {
	RankedPeople     int        `json:"ranked_people"`
	Missing          int        `json:"missing"`
	Dirty            int        `json:"dirty"`
	Outdated         int        `json:"outdated"`
	OldestComputedAt *time.Time `json:"oldest_computed_at"`
}
type WhoKnowsTimings struct {
	QueryEmbeddingMS   float64 `json:"query_embedding_ms"`
	RetrievalMS        float64 `json:"retrieval_ms"`
	AttributionMS      float64 `json:"attribution_ms"`
	DeclaredMS         float64 `json:"declared_ms"`
	ProfileMS          float64 `json:"profile_ms"`
	ProfileEmbeddingMS float64 `json:"profile_embedding_ms"`
	TiebreakMS         float64 `json:"tiebreak_ms"`
	HydrationMS        float64 `json:"hydration_ms"`
}
type WhoKnowsRetrieval struct {
	Mode                   string     `json:"mode"`
	CandidatesRequested    int        `json:"candidates_requested"`
	CandidatesEffective    int        `json:"candidates_effective"`
	CandidatesAfterFilters int        `json:"candidates_after_filters"`
	PoolSaturated          bool       `json:"pool_saturated"`
	Underfilled            bool       `json:"underfilled"`
	TopVectorScore         *float64   `json:"top_vector_score"`
	Generation             Generation `json:"generation"`
	Accelerator            string     `json:"accelerator"`
	TookMS                 float64    `json:"took_ms"`
	Timings                Timings    `json:"timings"`
}
type WhoKnowsTiebreak struct {
	Requested string `json:"requested"`
	Applied   string `json:"applied"`
}
type WhoKnowsResponse struct {
	Topic             string              `json:"topic"`
	GeneratedAt       time.Time           `json:"generated_at"`
	Reason            *string             `json:"reason"`
	Params            Params              `json:"params"`
	LimitsApplied     []string            `json:"limits_applied"`
	People            []Ranked            `json:"people"`
	Organizations     []OrganizationGroup `json:"organizations,omitzero"`
	UnsavedCandidates []UnsavedCandidate  `json:"unsaved_candidates"`
	Excluded          Excluded            `json:"excluded"`
	Coverage          Coverage            `json:"coverage"`
	Retrieval         Retrieval           `json:"retrieval"`
	Tiebreak          Tiebreak            `json:"tiebreak"`
}

// Evidence and the aliases below keep the ranking API concise and schema names unique.
type Evidence = WhoKnowsEvidence
type Excerpt = WhoKnowsExcerpt
type Breakdown = WhoKnowsBreakdown
type Organization = WhoKnowsOrganization
type TieValue = WhoKnowsTieValue
type Ranked = WhoKnowsRanked
type OrganizationGroup = WhoKnowsOrganizationGroup
type Excluded = WhoKnowsExcluded
type Generation = WhoKnowsGeneration
type Coverage = WhoKnowsCoverage
type Scope = WhoKnowsScope
type MessageCoverage = WhoKnowsMessageCoverage
type PeopleCoverage = WhoKnowsPeopleCoverage
type ContactCoverage = WhoKnowsContactCoverage
type Timings = WhoKnowsTimings
type Retrieval = WhoKnowsRetrieval
type Tiebreak = WhoKnowsTiebreak
type Response = WhoKnowsResponse

type WhoKnowsUnsavedBreakdown struct {
	Evidence            float64    `json:"evidence"`
	Depth               float64    `json:"depth"`
	RawAuthored         float64    `json:"raw_authored"`
	RawReceived         float64    `json:"raw_received"`
	Breadth             float64    `json:"breadth"`
	ProfileSearch       string     `json:"profile_search"`
	ProfileScore        *float64   `json:"profile_score"`
	ProfileSim          float64    `json:"profile_sim"`
	OrgMatch            float64    `json:"org_match"`
	AuthoredMessages    int        `json:"authored_messages"`
	ReceivedMessages    int        `json:"received_messages"`
	EvidenceTotal       int        `json:"evidence_total"`
	ShownContribution   float64    `json:"shown_contribution"`
	OmittedContribution float64    `json:"omitted_contribution"`
	ThreadsAuthored     int        `json:"threads_authored"`
	ChannelsAuthored    []string   `json:"channels_authored"`
	LatestAuthoredAt    *time.Time `json:"latest_authored_at"`
	AskMeAboutMatches   []string   `json:"ask_me_about_matches"`
	OrganizationMatches []string   `json:"organization_matches"`
}

type WhoKnowsUnsavedCandidate struct {
	ParticipantID int64                    `json:"participant_id"`
	DisplayName   string                   `json:"display_name"`
	EmailAddress  string                   `json:"email_address"`
	Score         float64                  `json:"score"`
	Flags         []string                 `json:"flags"`
	Breakdown     WhoKnowsUnsavedBreakdown `json:"breakdown"`
	Evidence      []Evidence               `json:"evidence"`
	PromoteHint   string                   `json:"promote_hint"`
}
type UnsavedCandidate = WhoKnowsUnsavedCandidate

func unsavedResult(r Ranked) UnsavedCandidate {
	b := r.Breakdown
	return UnsavedCandidate{ParticipantID: r.ParticipantID, DisplayName: r.DisplayName, EmailAddress: r.EmailAddress, Score: r.Score, Flags: r.Flags, Evidence: r.Evidence, PromoteHint: r.PromoteHint, Breakdown: WhoKnowsUnsavedBreakdown{Evidence: b.Evidence, Depth: b.Depth, RawAuthored: b.RawAuthored, RawReceived: b.RawReceived, Breadth: b.Breadth, ProfileSearch: b.ProfileSearch, ProfileScore: b.ProfileScore, ProfileSim: b.ProfileSim, OrgMatch: b.OrgMatch, AuthoredMessages: b.AuthoredMessages, ReceivedMessages: b.ReceivedMessages, EvidenceTotal: b.EvidenceTotal, ShownContribution: b.ShownContribution, OmittedContribution: b.OmittedContribution, ThreadsAuthored: b.ThreadsAuthored, ChannelsAuthored: b.ChannelsAuthored, LatestAuthoredAt: b.LatestAuthoredAt, AskMeAboutMatches: b.AskMeAboutMatches, OrganizationMatches: b.OrganizationMatches}}
}
