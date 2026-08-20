// Package personscope owns the shared identity and direction contract used to
// filter message-owned attachment occurrences across retrieval lanes.
package personscope

// Direction selects how an attachment's owning message relates to the
// requested person. A search may select several directions; the population is
// their union.
type Direction string

const (
	FromPerson Direction = "from_person"
	ToPerson   Direction = "to_person"
	Group      Direction = "group"
)

// Role is the exact message edge that matched one resolved participant.
type Role string

const (
	RoleFrom               Role = "from"
	RoleTo                 Role = "to"
	RoleCC                 Role = "cc"
	RoleBCC                Role = "bcc"
	RoleConversationMember Role = "conversation_member"
)

// Scope is an internal person constraint. The public resolver rejects an empty
// ParticipantIDs population; SQL builders still short-circuit directly-built
// empty scopes instead of emitting an invalid VALUES or IN expression.
type Scope struct {
	ParticipantIDs []int64     `json:"participant_ids"`
	Directions     []Direction `json:"directions"`
	// IncludeUnclassifiedRosterRows preserves the historical default for
	// conversation types without a dedicated direction selector.
	IncludeUnclassifiedRosterRows bool `json:"include_unclassified_roster_rows"`
}

// Provenance retains every matched participant and exact role on an
// attachment's owning message. Arrays use stable enum order.
type Provenance struct {
	ParticipantIDs []int64     `json:"participant_ids"`
	Roles          []Role      `json:"roles" enum:"from,to,cc,bcc,conversation_member"`
	Directions     []Direction `json:"directions" enum:"from_person,to_person,group"`
}
