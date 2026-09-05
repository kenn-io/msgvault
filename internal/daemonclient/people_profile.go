package daemonclient

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var _ peoplebrowser.ProfileReader = (*PeopleBrowser)(nil)

// maxProfileOrganizationLookups bounds the per-organization name resolution
// a single profile read may perform beyond the primary-employment projection.
const maxProfileOrganizationLookups = 10

// GetPersonProfile assembles one durable person's overview from the daemon's
// existing authenticated person routes. The person lookup is authoritative: an
// unknown ID surfaces the daemon's person_profile_not_found error. Sub-resource
// reads that the daemon reports as absent degrade to empty values instead of
// failing the whole read.
func (b *PeopleBrowser) GetPersonProfile(
	ctx context.Context, personID int64,
) (*peoplebrowser.PersonProfile, error) {
	if personID < 1 {
		return nil, errors.New("person ID must be positive")
	}
	personResp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GetPersonProfileResp, error) {
			return client.GetPersonProfileWithResponse(ctx,
				&generated.GetPersonProfileRequestOptions{
					PathParams: &generated.GetPersonProfilePath{ID: personID},
				})
		})
	if err != nil {
		return nil, err
	}
	profile := &peoplebrowser.PersonProfile{Person: *personFromGenerated(personResp.JSON200)}

	tracking, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GetPersonTrackingResp, error) {
			return client.GetPersonTrackingWithResponse(ctx,
				&generated.GetPersonTrackingRequestOptions{
					PathParams: &generated.GetPersonTrackingPath{ID: personID},
				})
		})
	switch {
	case err == nil:
		tracked := tracking.JSON200.Tracked
		profile.Tracked = &tracked
	case !absentAPIResource(err):
		return nil, err
	}

	state, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GetPersonContactStateResp, error) {
			return client.GetPersonContactStateWithResponse(ctx,
				&generated.GetPersonContactStateRequestOptions{
					PathParams: &generated.GetPersonContactStatePath{ID: personID},
				})
		})
	switch {
	case err == nil:
		// A missing projection is an HTTP 200 response with zero ComputedAt.
		if !state.JSON200.ComputedAt.IsZero() {
			profile.ContactState = contactStateFromGenerated(*state.JSON200)
		}
	case !absentAPIResource(err):
		return nil, err
	}

	attributes, err := b.ListAttributes(ctx, personID)
	switch {
	case err == nil:
		profile.Attributes = attributes.Groups
	case !absentAPIResource(err):
		return nil, err
	}

	profile.Employments, err = b.currentEmployments(ctx, personID)
	if err != nil {
		return nil, err
	}

	relationships, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.ListPersonRelationshipsResp, error) {
			return client.ListPersonRelationshipsWithResponse(ctx,
				&generated.ListPersonRelationshipsRequestOptions{
					PathParams: &generated.ListPersonRelationshipsPath{ID: personID},
					Query:      &generated.ListPersonRelationshipsQuery{},
				})
		})
	switch {
	case err == nil:
		profile.Relationships = make([]peoplebrowser.PersonRelationshipSummary, 0, len(relationships.JSON200.Relationships))
		for _, view := range relationships.JSON200.Relationships {
			profile.Relationships = append(profile.Relationships, relationshipSummaryFromGenerated(view))
		}
	case !absentAPIResource(err):
		return nil, err
	}

	structured, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GetPersonStructuredProfileResp, error) {
			return client.GetPersonStructuredProfileWithResponse(ctx,
				&generated.GetPersonStructuredProfileRequestOptions{
					PathParams: &generated.GetPersonStructuredProfilePath{ID: personID},
				})
		})
	switch {
	case err == nil:
		applyStructuredProfile(profile, *structured.JSON200)
	case !absentAPIResource(err):
		return nil, err
	}
	return profile, nil
}

// currentEmployments lists current employments and resolves organization
// names: the primary projection carries its own, and a bounded number of the
// remaining organizations are fetched individually.
func (b *PeopleBrowser) currentEmployments(
	ctx context.Context, personID int64,
) ([]peoplebrowser.PersonEmployment, error) {
	currentOnly := true
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.ListPersonEmploymentsResp, error) {
			return client.ListPersonEmploymentsWithResponse(ctx,
				&generated.ListPersonEmploymentsRequestOptions{
					PathParams: &generated.ListPersonEmploymentsPath{ID: personID},
					Query:      &generated.ListPersonEmploymentsQuery{CurrentOnly: &currentOnly},
				})
		})
	if err != nil {
		if absentAPIResource(err) {
			return []peoplebrowser.PersonEmployment{}, nil
		}
		return nil, err
	}
	names := map[int64]string{}
	if projection := resp.JSON200.Projection; projection != nil {
		names[projection.OrganizationID] = projection.OrganizationName
	}
	employments := make([]peoplebrowser.PersonEmployment, 0, len(resp.JSON200.Employments))
	lookups := 0
	for _, row := range resp.JSON200.Employments {
		if _, known := names[row.OrganizationID]; !known && lookups < maxProfileOrganizationLookups {
			lookups++
			names[row.OrganizationID] = b.organizationName(ctx, row.OrganizationID)
		}
		employments = append(employments, peoplebrowser.PersonEmployment{
			EmploymentID: row.ID, OrganizationID: row.OrganizationID,
			OrganizationName: names[row.OrganizationID],
			Title:            stringValue(row.Title), Role: stringValue(row.Role),
			Department: stringValue(row.Department), Location: stringValue(row.Location),
			StartDate: partialDateString(row.StartDate), EndDate: partialDateString(row.EndDate),
			IsCurrent: row.IsCurrent, IsPrimary: row.IsPrimary,
			Source: store.Provenance(row.Source),
		})
	}
	sort.SliceStable(employments, func(i, j int) bool {
		return employments[i].IsPrimary && !employments[j].IsPrimary
	})
	return employments, nil
}

// organizationName resolves one organization's display name, returning an
// empty string when the daemon cannot serve it so a name gap never fails the
// profile read.
func (b *PeopleBrowser) organizationName(ctx context.Context, organizationID int64) string {
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GetOrganizationResp, error) {
			return client.GetOrganizationWithResponse(ctx,
				&generated.GetOrganizationRequestOptions{
					PathParams: &generated.GetOrganizationPath{ID: organizationID},
				})
		})
	if err != nil || resp.JSON200 == nil {
		return ""
	}
	return resp.JSON200.Organization.Name
}

// absentAPIResource reports a daemon answer that means "nothing to show" for
// an optional sub-resource: not found, or a feature store the daemon has not
// enabled.
func absentAPIResource(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusServiceUnavailable)
}

func contactStateFromGenerated(state generated.ContactState) *store.ContactState {
	return &store.ContactState{
		PersonID:            state.PersonID,
		FirstContactAt:      copyTime(state.FirstContactAt),
		FirstContactRef:     stringValue(state.FirstContactRef),
		LastContactAt:       copyTime(state.LastContactAt),
		LastContactRef:      stringValue(state.LastContactRef),
		LastContactChannel:  store.ActivityChannel(stringValue(state.LastContactChannel)),
		LastContactSourceID: copyInt64(state.LastContactSourceID),
		LastContactOwner:    stringValue(state.LastContactOwner),
		LastInboundAt:       copyTime(state.LastInboundAt),
		LastInboundRef:      stringValue(state.LastInboundRef),
		LastOutboundAt:      copyTime(state.LastOutboundAt),
		LastOutboundRef:     stringValue(state.LastOutboundRef),
		InteractionCount:    state.InteractionCount,
		InferredChannel:     store.ActivityChannel(stringValue(state.InferredChannel)),
		CadenceDueAt:        copyTime(state.CadenceDueAt),
		CadenceStatus:       state.CadenceStatus,
		Stale:               state.Stale,
		ComputedAt:          state.ComputedAt,
	}
}

func relationshipSummaryFromGenerated(view generated.PersonRelationshipView) peoplebrowser.PersonRelationshipSummary {
	return peoplebrowser.PersonRelationshipSummary{
		RelationshipID:         view.Relationship.ID,
		TypeSlug:               view.Relationship.TypeSlug,
		CounterpartLabel:       view.CounterpartLabel,
		CounterpartPersonID:    view.CounterpartPersonID,
		CounterpartDisplayName: stringValue(view.CounterpartDisplayName),
		Direction:              view.Direction,
		Status:                 view.Relationship.Status,
		StartDate:              partialDateString(view.Relationship.StartDate),
		EndDate:                partialDateString(view.Relationship.EndDate),
		Source:                 store.Provenance(view.Relationship.Source),
	}
}

// applyStructuredProfile copies the current contact points, dates, and
// categories. Addresses, names, and media stay out of the overview: the
// display name already comes from the person record, and addresses and media
// are not needed to answer who a person is or how to reach them.
func applyStructuredProfile(profile *peoplebrowser.PersonProfile, structured generated.StructuredPersonProfile) {
	profile.ContactPoints = make([]peoplebrowser.PersonContactPointSummary, 0, len(structured.ContactPoints))
	for _, point := range structured.ContactPoints {
		if !currentEnvelope(point.Envelope) {
			continue
		}
		profile.ContactPoints = append(profile.ContactPoints, peoplebrowser.PersonContactPointSummary{
			Kind: point.AddressKind, Value: point.OriginalValue,
			ServiceSlug: stringValue(point.ServiceSlug), URI: stringValue(point.URI),
			TypeLabel: stringValue(point.Envelope.TypeLabel),
			Preferred: point.Envelope.Pref != nil && *point.Envelope.Pref == 1,
			Source:    store.Provenance(point.Envelope.Source),
		})
	}
	profile.Dates = make([]peoplebrowser.PersonDateSummary, 0, len(structured.Dates))
	for _, date := range structured.Dates {
		if !currentEnvelope(date.Envelope) {
			continue
		}
		rendered := partialDateString(&date.Date)
		if rendered == "" {
			rendered = stringValue(date.DateText)
		}
		profile.Dates = append(profile.Dates, peoplebrowser.PersonDateSummary{
			Kind: date.DateKind, Date: rendered, Label: stringValue(date.Label),
			Source: store.Provenance(date.Envelope.Source),
		})
	}
	profile.Categories = make([]string, 0, len(structured.Categories))
	for _, category := range structured.Categories {
		if !currentEnvelope(category.Envelope) {
			continue
		}
		profile.Categories = append(profile.Categories, category.OriginalValue)
	}
}

func currentEnvelope(envelope generated.ValueEnvelope) bool {
	return envelope.ActiveUntil == nil && envelope.SupersededAt == nil
}

func partialDateString(date *generated.PartialDate) string {
	if date == nil {
		return ""
	}
	converted := store.PartialDate{}
	if date.Year != nil {
		year := int(*date.Year)
		converted.Year = &year
	}
	if date.Month != nil {
		month := int(*date.Month)
		converted.Month = &month
	}
	if date.Day != nil {
		day := int(*date.Day)
		converted.Day = &day
	}
	return converted.String()
}
