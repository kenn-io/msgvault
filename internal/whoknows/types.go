package whoknows

import "time"

// WhoKnowsRequest asks who in the archive has written about a topic.
// Omitted limit, evidence, and min_score take their defaults.
type WhoKnowsRequest struct {
	Topic    string   `json:"topic" minLength:"1" doc:"Free-text topic"`
	Limit    *int     `json:"limit,omitempty" minimum:"1" maximum:"50" doc:"Saved people to return; default 10"`
	Evidence *int     `json:"evidence,omitempty" minimum:"1" maximum:"10" doc:"Citations per person; default 3"`
	MinScore *float64 `json:"min_score,omitempty" exclusiveMinimum:"0" maximum:"1" doc:"Lowest meaning similarity a message needs to count; default 0.5"`
	After    string   `json:"after,omitempty" doc:"YYYY-MM-DD or RFC3339; messages at or after this instant"`
	Before   string   `json:"before,omitempty" doc:"YYYY-MM-DD or RFC3339; messages before this instant"`
}

// WhoKnowsCitation is one message a person wrote about the topic.
type WhoKnowsCitation struct {
	MessageID   int64      `json:"message_id"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	MessageType string     `json:"message_type"`
	Subject     string     `json:"subject,omitempty"`
	Snippet     string     `json:"snippet,omitempty"`
}

// WhoKnowsPerson is a saved person ranked for the topic.
type WhoKnowsPerson struct {
	Rank             int                `json:"rank"`
	PersonID         int64              `json:"person_id"`
	DisplayName      string             `json:"display_name,omitempty"`
	Score            float64            `json:"score"`
	AuthoredMessages int                `json:"authored_messages"`
	AskMeAbout       []string           `json:"ask_me_about,omitempty"`
	Citations        []WhoKnowsCitation `json:"citations"`
}

// WhoKnowsContact is a sender bound to no saved person. Promote it with
// "msgvault person promote <participant_id>".
type WhoKnowsContact struct {
	Rank             int                `json:"rank"`
	ParticipantID    int64              `json:"participant_id"`
	DisplayName      string             `json:"display_name,omitempty"`
	EmailAddress     string             `json:"email_address,omitempty"`
	Score            float64            `json:"score"`
	AuthoredMessages int                `json:"authored_messages"`
	Citations        []WhoKnowsCitation `json:"citations"`
}

// WhoKnowsResponse lists people first, then unsaved contacts. Reason is
// "no_relevant_evidence" when both lists are empty.
type WhoKnowsResponse struct {
	Topic           string            `json:"topic"`
	Reason          string            `json:"reason,omitempty"`
	People          []WhoKnowsPerson  `json:"people"`
	UnsavedContacts []WhoKnowsContact `json:"unsaved_contacts"`
}

// ReasonNoEvidence marks a response with nobody to list.
const ReasonNoEvidence = "no_relevant_evidence"

type (
	Request  = WhoKnowsRequest
	Response = WhoKnowsResponse
	Person   = WhoKnowsPerson
	Contact  = WhoKnowsContact
	Citation = WhoKnowsCitation
)
