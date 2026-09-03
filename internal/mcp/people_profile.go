package mcp

import (
	"context"
	"errors"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
)

// getPersonProfileResponse answers "who is this", "when did we last talk",
// and "which network do I reach them on" for one durable person from local
// derived state. Sensitive attributes and private Notes are never included.
type getPersonProfileResponse struct {
	PersonID    int64  `json:"person_id"`
	DisplayName string `json:"display_name"`
	VCardUID    string `json:"vcard_uid"`
	Tracked     *bool  `json:"tracked"`
	// ContactState is the deterministic activity projection; null until the
	// activity job has computed a row for this person.
	ContactState *store.ContactState `json:"contact_state"`
	// PrimaryChannel is the curated primary_channel attribute when set.
	PrimaryChannel string `json:"primary_channel,omitempty"`
	// InferredChannel is the channel the activity projection derived from
	// archived interactions; it is a stored observation, not a preference.
	InferredChannel string                      `json:"inferred_channel,omitempty"`
	Attributes      []personProfileAttribute    `json:"attributes"`
	Employments     []personProfileEmployment   `json:"employments"`
	Relationships   []personProfileRelationship `json:"relationships"`
	ContactPoints   []personProfileContactPoint `json:"contact_points"`
	Dates           []personProfileDate         `json:"dates"`
	Categories      []string                    `json:"categories"`
	// Excluded names the classes of profile data this tool deliberately omits.
	Excluded []string `json:"excluded"`
}

type personProfileAttribute struct {
	Slug        string                       `json:"slug"`
	Label       string                       `json:"label"`
	UniversalID string                       `json:"universal_id"`
	ValueType   store.AttributeValueType     `json:"value_type"`
	Cardinality store.AttributeCardinality   `json:"cardinality"`
	Current     []store.PersonAttributeValue `json:"current"`
}

type personProfileEmployment struct {
	EmploymentID     int64            `json:"employment_id"`
	OrganizationID   int64            `json:"organization_id"`
	OrganizationName string           `json:"organization_name,omitempty"`
	Title            string           `json:"title,omitempty"`
	Role             string           `json:"role,omitempty"`
	Department       string           `json:"department,omitempty"`
	Location         string           `json:"location,omitempty"`
	StartDate        string           `json:"start_date,omitempty"`
	EndDate          string           `json:"end_date,omitempty"`
	IsCurrent        bool             `json:"is_current"`
	IsPrimary        bool             `json:"is_primary"`
	Source           store.Provenance `json:"source"`
}

type personProfileRelationship struct {
	RelationshipID         int64            `json:"relationship_id"`
	TypeSlug               string           `json:"type_slug"`
	CounterpartLabel       string           `json:"counterpart_label"`
	CounterpartPersonID    int64            `json:"counterpart_person_id"`
	CounterpartDisplayName string           `json:"counterpart_display_name,omitempty"`
	Direction              string           `json:"direction"`
	Status                 string           `json:"status"`
	StartDate              string           `json:"start_date,omitempty"`
	EndDate                string           `json:"end_date,omitempty"`
	Source                 store.Provenance `json:"source"`
}

type personProfileContactPoint struct {
	Kind        string           `json:"kind"`
	Value       string           `json:"value"`
	ServiceSlug string           `json:"service_slug,omitempty"`
	URI         string           `json:"uri,omitempty"`
	TypeLabel   string           `json:"type_label,omitempty"`
	Preferred   bool             `json:"preferred"`
	Source      store.Provenance `json:"source"`
}

type personProfileDate struct {
	Kind   string           `json:"kind"`
	Date   string           `json:"date"`
	Label  string           `json:"label,omitempty"`
	Source store.Provenance `json:"source"`
}

var personProfileExcluded = []string{"sensitive_attributes", "notes", "addresses", "media"}

const personProfileNotFoundMessage = "person profile not found; use search_people to find a durable person_id, or promote_person to create one from an observed participant"

func getPersonProfileDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolGetPersonProfile,
		"Read one durable person's overview from local derived state: display name, tracking, contact state (first/last contact, last inbound and outbound, interaction count, inferred channel), curated primary channel, non-sensitive attributes, current employment, typed relationships, contact points, dates, and categories. Sensitive attributes, private Notes, addresses, and media are excluded. Makes no provider calls.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgPersonID: safeIDSchema("Durable person profile ID"),
		}, toolArgPersonID),
		outputSchemaFor[getPersonProfileResponse](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.getPersonProfile(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func (h *handlers) getPersonProfile(ctx context.Context, req toolRequest) (*toolResult, error) {
	personID, err := requiredPeopleID(req.GetArguments(), toolArgPersonID)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	reader, ok := h.peopleBackend.(peoplebrowser.ProfileReader)
	if !ok {
		return nil, newInternalError("get person profile", errors.New("durable profile reads are unavailable"))
	}
	profile, err := reader.GetPersonProfile(ctx, personID)
	if err != nil {
		if personProfileMissing(err) {
			return toolErrorResult(personProfileNotFoundMessage), nil
		}
		return nil, newInternalError("get person profile", err)
	}
	if profile == nil {
		return nil, newInternalError("get person profile", errors.New("empty response"))
	}
	return jsonResult(personProfileResponse(*profile))
}

func personProfileResponse(profile peoplebrowser.PersonProfile) getPersonProfileResponse {
	response := getPersonProfileResponse{
		PersonID:      profile.Person.ID,
		DisplayName:   profileDisplayLabel(profile.Person),
		VCardUID:      profile.Person.VCardUID,
		Tracked:       profile.Tracked,
		ContactState:  profile.ContactState,
		Attributes:    []personProfileAttribute{},
		Employments:   []personProfileEmployment{},
		Relationships: []personProfileRelationship{},
		ContactPoints: []personProfileContactPoint{},
		Dates:         []personProfileDate{},
		Categories:    []string{},
		Excluded:      append([]string(nil), personProfileExcluded...),
	}
	if profile.ContactState != nil {
		response.InferredChannel = string(profile.ContactState.InferredChannel)
	}
	for _, group := range profile.Attributes {
		definition := group.Definition
		if definition.IsSensitive || definition.UniversalID == store.AttributeUniversalIDNotes {
			continue
		}
		if definition.UniversalID == store.AttributeUniversalIDPrimaryChannel {
			for _, value := range group.Current {
				if value.Value.Text != nil && strings.TrimSpace(*value.Value.Text) != "" {
					response.PrimaryChannel = *value.Value.Text
					break
				}
			}
		}
		current := group.Current
		if current == nil {
			current = []store.PersonAttributeValue{}
		}
		response.Attributes = append(response.Attributes, personProfileAttribute{
			Slug: definition.Slug, Label: definition.Label, UniversalID: definition.UniversalID,
			ValueType: definition.ValueType, Cardinality: definition.Cardinality, Current: current,
		})
	}
	for _, employment := range profile.Employments {
		response.Employments = append(response.Employments, personProfileEmployment{
			EmploymentID: employment.EmploymentID, OrganizationID: employment.OrganizationID,
			OrganizationName: employment.OrganizationName, Title: employment.Title,
			Role: employment.Role, Department: employment.Department, Location: employment.Location,
			StartDate: employment.StartDate, EndDate: employment.EndDate,
			IsCurrent: employment.IsCurrent, IsPrimary: employment.IsPrimary, Source: employment.Source,
		})
	}
	for _, relationship := range profile.Relationships {
		response.Relationships = append(response.Relationships, personProfileRelationship{
			RelationshipID: relationship.RelationshipID, TypeSlug: relationship.TypeSlug,
			CounterpartLabel: relationship.CounterpartLabel, CounterpartPersonID: relationship.CounterpartPersonID,
			CounterpartDisplayName: relationship.CounterpartDisplayName, Direction: relationship.Direction,
			Status: relationship.Status, StartDate: relationship.StartDate, EndDate: relationship.EndDate,
			Source: relationship.Source,
		})
	}
	for _, point := range profile.ContactPoints {
		response.ContactPoints = append(response.ContactPoints, personProfileContactPoint{
			Kind: point.Kind, Value: point.Value, ServiceSlug: point.ServiceSlug, URI: point.URI,
			TypeLabel: point.TypeLabel, Preferred: point.Preferred, Source: point.Source,
		})
	}
	for _, date := range profile.Dates {
		response.Dates = append(response.Dates, personProfileDate{
			Kind: date.Kind, Date: date.Date, Label: date.Label, Source: date.Source,
		})
	}
	if len(profile.Categories) > 0 {
		response.Categories = append(response.Categories, profile.Categories...)
	}
	return response
}
