package peoplebrowser

import (
	"context"
	"time"
)

// PersonBriefReader is the optional read-only surface for the "last time we
// talked" brief. A person with no current version is not an error: the reader
// returns a nil brief so the rest of the person still renders.
type PersonBriefReader interface {
	GetPersonBrief(ctx context.Context, personID int64) (*PersonBrief, error)
}

// PersonBriefWriter is the optional brief mutation surface: the enrollment
// opt-in that sits on top of tracking, a manual generation, and the owner's
// rejection of the current version. Every mutation spends the same consented
// provider profile the people sweep uses.
type PersonBriefWriter interface {
	SetPersonBriefEnrollment(
		ctx context.Context, personID int64, enrolled, track bool,
	) (*PersonBriefEnrollment, error)
	GeneratePersonBrief(ctx context.Context, personID int64) (*PersonBriefRun, error)
	RejectPersonBrief(ctx context.Context, personID int64, reason string) (*PersonBrief, error)
}

// PersonBriefItemKind names the structured item one rendered sentence came
// from. The values are the wire values the API and the sweep's renderer use.
type PersonBriefItemKind string

const (
	PersonBriefLastInteraction PersonBriefItemKind = "last_interaction"
	PersonBriefHighlight       PersonBriefItemKind = "highlight"
	PersonBriefFollowUp        PersonBriefItemKind = "follow_up"
	PersonBriefAppreciation    PersonBriefItemKind = "appreciation"
	PersonBriefUncertainty     PersonBriefItemKind = "uncertainty"
)

// PersonBrief is one immutable brief version as a reader consumes it: the
// paragraph the owner scans, the sentence-to-item map, and the validated
// structure with the archive items each item cites. It never carries an
// excerpt of the archive text the brief was derived from.
type PersonBrief struct {
	Version          int
	Status           string
	GeneratedAt      time.Time
	RenderedText     string
	Sentences        []PersonBriefSentence
	Items            []PersonBriefItem
	DroppedItemCount int
	RejectedAt       *time.Time
	RejectedReason   string
	// Evidence retains the whole brief's citations when sentence links are unavailable.
	Evidence []PersonBriefEvidence
}

// PersonBriefSentence is one rendered sentence and the structured item it came
// from. Index is the item's position within its kind. EvidenceOrdinals names
// the entries of the version's evidence list this sentence cites, and is empty
// when the source of the brief could not supply the join.
type PersonBriefSentence struct {
	Kind             PersonBriefItemKind
	Index            int
	Text             string
	EvidenceOrdinals []int
}

// PersonBriefItem is one validated structured item in rendering order.
// Speaker is set for a highlight, Why for a follow-up, and Reason for an
// uncertainty; the other kinds leave them empty. Evidence is empty when the
// citation order behind the brief could not be rebuilt.
type PersonBriefItem struct {
	Kind     PersonBriefItemKind
	Index    int
	Text     string
	Speaker  string
	Why      string
	Reason   string
	Evidence []PersonBriefEvidence
}

// PersonBriefEvidence is one cited archive item. Supported is false once a
// later status event invalidated the evidence's source; the reader shows the
// item as unsupported rather than hiding it.
type PersonBriefEvidence struct {
	Ordinal    int
	EvidenceID int64
	SourceRef  string
	SourceURL  string
	Directness string
	EventTime  time.Time
	Supported  bool
}

// PersonBriefEnrollment is a person's brief opt-in. EnabledAt is nil when the
// person is not enrolled.
type PersonBriefEnrollment struct {
	PersonID  int64
	Enrolled  bool
	EnabledAt *time.Time
	Actor     string
}

// PersonBriefRun is the outcome of one manual generation. BriefVersion is the
// version the attempt stored, or zero; BriefFailureClass says why there is
// none when the attempt itself succeeded.
type PersonBriefRun struct {
	RunID             string
	AttemptID         string
	BriefVersion      int
	BriefFailureClass string
}
