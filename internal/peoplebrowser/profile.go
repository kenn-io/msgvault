package peoplebrowser

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

// ProfileReader is the optional read-only person overview surface. It
// assembles local derived state for one durable person: identity, contact
// state, curated attributes, employment, relationships, and the structured
// contact points, dates, and categories the authenticated HTTP API already
// exposes. It never contacts a hosted provider.
type ProfileReader interface {
	GetPersonProfile(ctx context.Context, personID int64) (*PersonProfile, error)
}

// PersonProfile is one durable person's overview. Nil ContactState means the
// activity projection has not computed a row for the person yet; Tracked is
// nil when the daemon could not report tracking state.
type PersonProfile struct {
	Person        store.Person
	Tracked       *bool
	ContactState  *store.ContactState
	Attributes    []AttributeGroup
	Employments   []PersonEmployment
	Relationships []PersonRelationshipSummary
	ContactPoints []PersonContactPointSummary
	Dates         []PersonDateSummary
	Categories    []string
}

// PersonEmployment is one current employment rendered for reading.
// OrganizationName is empty when the daemon could not resolve it.
type PersonEmployment struct {
	EmploymentID     int64
	OrganizationID   int64
	OrganizationName string
	Title            string
	Role             string
	Department       string
	Location         string
	StartDate        string
	EndDate          string
	IsCurrent        bool
	IsPrimary        bool
	Source           store.Provenance
}

// PersonRelationshipSummary is one typed relationship rendered from the
// person's side: CounterpartLabel names what the counterpart is to the person
// (for example "partner" or "child").
type PersonRelationshipSummary struct {
	RelationshipID         int64
	TypeSlug               string
	CounterpartLabel       string
	CounterpartPersonID    int64
	CounterpartDisplayName string
	Direction              string
	Status                 string
	StartDate              string
	EndDate                string
	Source                 store.Provenance
}

// PersonContactPointSummary is one current structured contact point.
type PersonContactPointSummary struct {
	Kind        string
	Value       string
	ServiceSlug string
	URI         string
	TypeLabel   string
	Preferred   bool
	Source      store.Provenance
}

// PersonDateSummary is one current structured date such as a birthday.
type PersonDateSummary struct {
	Kind   string
	Date   string
	Label  string
	Source store.Provenance
}
