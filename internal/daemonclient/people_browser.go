package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PeopleBrowser adapts the daemon-backed query engine to the People TUI
// contract. It is separate from Engine because query.Engine already owns a
// Search method with a different signature.
type PeopleBrowser struct {
	engine *Engine
}

var _ peoplebrowser.Backend = (*PeopleBrowser)(nil)

// NewPeopleBrowser constructs a People backend over an existing daemon
// engine, sharing its configured client and request lifecycle.
func NewPeopleBrowser(engine *Engine) *PeopleBrowser {
	return &PeopleBrowser{engine: engine}
}

func (b *PeopleBrowser) Search(
	ctx context.Context, request peoplebrowser.SearchRequest,
) (*peoplebrowser.SearchPage, error) {
	body := generated.SearchParticipantsBody{
		IdentityQuery: optionalString(request.Query),
		Cursor:        optionalString(request.Cursor),
		Limit:         optionalPositiveInt64(request.Limit),
		Sort: generated.IdentitySearchSort{
			Field:     generated.IdentitySearchSortFieldActivityCount,
			Direction: generated.IdentitySearchSortDirectionDesc,
		},
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.SearchParticipantsResp, error) {
			return client.SearchParticipantsWithResponse(ctx,
				&generated.SearchParticipantsRequestOptions{Body: &body})
		})
	if err != nil {
		return nil, err
	}
	page := &peoplebrowser.SearchPage{
		TotalCount:    resp.JSON200.TotalCount,
		NextCursor:    stringValue(resp.JSON200.NextCursor),
		CacheRevision: resp.JSON200.CacheRevision,
		Rows:          make([]query.PersonSummary, len(resp.JSON200.Rows)),
	}
	for i, row := range resp.JSON200.Rows {
		page.Rows[i] = personSummaryFromGenerated(row)
	}
	return page, nil
}

func (b *PeopleBrowser) Complete(
	ctx context.Context, request peoplebrowser.CompletionRequest,
) (*peoplebrowser.CompletionPage, error) {
	body := generated.CompleteParticipantsBody{
		Query: request.Query,
		Limit: optionalPositiveInt64(request.Limit),
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.CompleteParticipantsResp, error) {
			return client.CompleteParticipantsWithResponse(ctx,
				&generated.CompleteParticipantsRequestOptions{Body: &body})
		})
	if err != nil {
		return nil, err
	}
	page := &peoplebrowser.CompletionPage{
		Rows:          make([]peoplebrowser.CompletionRow, len(resp.JSON200.Rows)),
		CacheRevision: resp.JSON200.CacheRevision,
	}
	for i, row := range resp.JSON200.Rows {
		page.Rows[i] = peoplebrowser.CompletionRow{
			ParticipantID: row.ParticipantID,
			DisplayLabel:  row.DisplayLabel,
			Kind:          query.PeopleCompletionKind(row.Kind),
			Value:         row.Value,
			Source:        row.Source,
		}
	}
	return page, nil
}

func (b *PeopleBrowser) GetContact(
	ctx context.Context, participantID int64,
) (*query.PersonSummary, error) {
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GetParticipantResp, error) {
			return client.GetParticipantWithResponse(ctx,
				&generated.GetParticipantRequestOptions{
					PathParams: &generated.GetParticipantPath{ID: participantID},
				})
		})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound &&
			apiErr.Code == "participant_not_found" {
			return nil, fmt.Errorf("%w: %v", peoplebrowser.ErrContactNotFound, err)
		}
		return nil, err
	}
	person := personSummaryFromGenerated(*resp.JSON200)
	return &person, nil
}

func (b *PeopleBrowser) Promote(
	ctx context.Context, participantID int64,
) (*store.Person, error) {
	resp, err := APIResponseWithStatuses(b.engine.store,
		[]int{http.StatusOK, http.StatusCreated},
		func(client *apiclient.Client) (*generated.CreatePersonResp, error) {
			return client.CreatePersonWithResponse(ctx,
				&generated.CreatePersonRequestOptions{
					Body: &generated.CreatePersonBody{ParticipantID: participantID},
				})
		})
	if err != nil {
		return nil, err
	}
	person := resp.JSON200
	if person == nil {
		person = resp.JSON201
	}
	return personFromGenerated(person), nil
}

func (b *PeopleBrowser) ListAttributes(
	ctx context.Context, personID int64,
) (*peoplebrowser.Attributes, error) {
	return b.listAttributes(ctx, personID, "")
}

// ListAttributesBySlug returns one definition group through the same daemon
// facade. It keeps focused CLI reads from transferring unrelated private
// profile attributes without changing the broader TUI backend contract.
func (b *PeopleBrowser) ListAttributesBySlug(
	ctx context.Context, personID int64, slug string,
) (*peoplebrowser.Attributes, error) {
	return b.listAttributes(ctx, personID, slug)
}

func (b *PeopleBrowser) listAttributes(
	ctx context.Context, personID int64, slug string,
) (*peoplebrowser.Attributes, error) {
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.ListPersonAttributesResp, error) {
			return client.ListPersonAttributesWithResponse(ctx,
				&generated.ListPersonAttributesRequestOptions{
					PathParams: &generated.ListPersonAttributesPath{ID: personID},
					Query: &generated.ListPersonAttributesQuery{
						Slug: optionalString(slug),
					},
				})
		})
	if err != nil {
		return nil, err
	}
	attributes := &peoplebrowser.Attributes{
		PersonID: resp.JSON200.PersonID,
		Groups:   make([]peoplebrowser.AttributeGroup, len(resp.JSON200.Attributes)),
	}
	for i, group := range resp.JSON200.Attributes {
		current := make([]store.PersonAttributeValue, len(group.Current))
		for j, value := range group.Current {
			current[j] = personAttributeValueFromGenerated(value)
		}
		attributes.Groups[i] = peoplebrowser.AttributeGroup{
			Definition: attributeDefinitionFromGenerated(group.Definition),
			Current:    current,
		}
	}
	return attributes, nil
}

func (b *PeopleBrowser) CreateField(
	ctx context.Context, field peoplebrowser.NewField,
) (*store.AttributeDefinition, error) {
	input, err := field.DefinitionInput()
	if err != nil {
		return nil, err
	}
	cardinality := generated.CreateAttributeDefinitionRequestCardinality(input.Cardinality)
	body := generated.CreateAttributeDefinitionBody{
		ObjectType:  generated.CreateAttributeDefinitionRequestObjectTypePerson,
		Label:       input.Label,
		ValueType:   string(input.ValueType),
		FieldType:   string(input.FieldType),
		Cardinality: &cardinality,
	}
	resp, err := APIResponseWithStatuses(b.engine.store,
		[]int{http.StatusCreated},
		func(client *apiclient.Client) (*generated.CreateAttributeDefinitionResp, error) {
			return client.CreateAttributeDefinitionWithResponse(ctx,
				&generated.CreateAttributeDefinitionRequestOptions{Body: &body})
		})
	if err != nil {
		return nil, err
	}
	definition := attributeDefinitionFromGenerated(*resp.JSON201)
	return &definition, nil
}

func (b *PeopleBrowser) SetAttribute(
	ctx context.Context, request peoplebrowser.SetAttributeRequest,
) (*store.PersonAttributeWrite, error) {
	source := generated.SetPersonAttributeRequestSourceUser
	body := generated.SetPersonAttributeBody{
		Value:           attributeValueToGenerated(request.Value),
		Ordinal:         request.Ordinal,
		ExpectedValueID: request.ExpectedValueID,
		Source:          &source,
	}
	resp, err := apiResponseWithErrorDecoder(b.engine.store,
		func(client *apiclient.Client) (*generated.SetPersonAttributeResp, error) {
			return client.SetPersonAttributeWithResponse(ctx,
				&generated.SetPersonAttributeRequestOptions{
					PathParams: &generated.SetPersonAttributePath{
						ID: request.PersonID, Slug: request.Slug,
					},
					Body: &body,
				})
		}, decodePersonAttributeError)
	if err != nil {
		return nil, err
	}
	write := personAttributeWriteFromGenerated(resp.JSON200)
	return &write, nil
}

func (b *PeopleBrowser) AppendNote(
	ctx context.Context, request peoplebrowser.AppendNoteRequest,
) (*store.PersonAttributeWrite, error) {
	source := generated.AppendPersonNoteRequestSourceUser
	body := generated.AppendPersonNoteBody{
		Text: request.Text, Source: &source, Actor: optionalString(request.Actor),
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.AppendPersonNoteResp, error) {
			return client.AppendPersonNoteWithResponse(ctx,
				&generated.AppendPersonNoteRequestOptions{
					PathParams: &generated.AppendPersonNotePath{ID: request.PersonID},
					Body:       &body,
				})
		})
	if err != nil {
		return nil, err
	}
	write := personAttributeWriteFromGenerated(resp.JSON200)
	return &write, nil
}

func decodePersonAttributeError(status int, body []byte) error {
	if status != http.StatusConflict {
		return handleErrorBody(status, body)
	}
	var conflict struct {
		Error          string                          `json:"error"`
		CurrentValueID int64                           `json:"current_value_id"`
		CurrentValue   *generated.PersonAttributeValue `json:"current_value"`
	}
	if json.Unmarshal(body, &conflict) != nil || conflict.Error != "attribute_value_conflict" {
		return handleErrorBody(status, body)
	}
	stale := peoplebrowser.StaleValueError{CurrentValueID: conflict.CurrentValueID}
	if conflict.CurrentValue != nil {
		current := personAttributeValueFromGenerated(*conflict.CurrentValue)
		stale.CurrentValue = &current
		if stale.CurrentValueID == 0 {
			stale.CurrentValueID = current.ID
		}
	}
	return stale
}

func (b *PeopleBrowser) ListInboxes(
	ctx context.Context, participantID int64,
) (*query.PersonInboxResponse, error) {
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.ListParticipantInboxesResp, error) {
			return client.ListParticipantInboxesWithResponse(ctx,
				&generated.ListParticipantInboxesRequestOptions{
					PathParams: &generated.ListParticipantInboxesPath{ID: participantID},
				})
		})
	if err != nil {
		return nil, err
	}
	inboxes := &query.PersonInboxResponse{
		Rows:             make([]query.PersonInboxRow, len(resp.JSON200.Rows)),
		CacheRevision:    resp.JSON200.CacheRevision,
		IdentityRevision: resp.JSON200.IdentityRevision,
	}
	for i, row := range resp.JSON200.Rows {
		inboxes.Rows[i] = query.PersonInboxRow{
			SourceID: row.SourceID, SourceType: row.SourceType,
			SourceIdentifier:  row.SourceIdentifier,
			ConversationCount: row.ConversationCount,
			ReceivedCount:     row.ReceivedCount, SentCount: row.SentCount,
			LatestReceivedAt: row.LatestReceivedAt, LatestSentAt: row.LatestSentAt,
			LatestAt: row.LatestAt,
		}
	}
	return inboxes, nil
}

func (b *PeopleBrowser) ListConversations(
	ctx context.Context, filter query.TextFilter,
) (*peoplebrowser.ConversationPage, error) {
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.ListTextConversationsResp, error) {
			return client.ListTextConversationsWithResponse(ctx,
				&generated.ListTextConversationsRequestOptions{Query: textConversationsQuery(filter)})
		})
	if err != nil {
		return nil, err
	}
	page := &peoplebrowser.ConversationPage{
		Rows:          textConversationRowsFromGenerated(resp.JSON200.Conversations),
		Complete:      !resp.JSON200.HasMore,
		CacheRevision: resp.JSON200.CacheRevision,
	}
	if resp.JSON200.HasMore {
		page.NextOffset = int(resp.JSON200.Offset + resp.JSON200.Limit)
	}
	return page, nil
}

func (b *PeopleBrowser) ListConversationMessages(
	ctx context.Context, conversationID int64, filter query.TextFilter,
) (*peoplebrowser.ConversationMessagePage, error) {
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.ListTextConversationMessagesResp, error) {
			return client.ListTextConversationMessagesWithResponse(ctx,
				&generated.ListTextConversationMessagesRequestOptions{
					PathParams: &generated.ListTextConversationMessagesPath{ID: conversationID},
					Query:      textConversationMessagesQuery(filter),
				})
		})
	if err != nil {
		return nil, err
	}
	page := &peoplebrowser.ConversationMessagePage{
		Rows:          queryMessageSummariesFromCLIGenerated(resp.JSON200.Messages),
		Complete:      !resp.JSON200.HasMore,
		CacheRevision: resp.JSON200.CacheRevision,
	}
	if resp.JSON200.HasMore {
		page.NextOffset = int(resp.JSON200.Offset + resp.JSON200.Limit)
	}
	return page, nil
}

func (b *PeopleBrowser) ListMeetings(
	ctx context.Context, request peoplebrowser.ContactPageRequest,
) (*peoplebrowser.MessagePage, error) {
	response, err := b.participantTimeline(ctx, request, []generated.ExploreFilter{{
		Dimension: generated.ExploreFilterDimensionMessageType,
		Values:    []string{"meeting_transcript"},
	}})
	if err != nil {
		return nil, err
	}
	page := &peoplebrowser.MessagePage{
		Rows:          make([]query.MessageSummary, len(response.Rows)),
		NextCursor:    stringValue(response.NextCursor),
		CacheRevision: response.CacheRevision,
	}
	for i, row := range response.Rows {
		if row.AnchorMessageID == nil {
			return nil, fmt.Errorf("meeting timeline row %q has no anchor message", row.Key)
		}
		page.Rows[i] = meetingSummaryFromGenerated(row)
	}
	return page, nil
}

func (b *PeopleBrowser) ListFiles(
	ctx context.Context, request peoplebrowser.ContactPageRequest,
) (*peoplebrowser.FilePage, error) {
	presentation := generated.ExploreHTTPRequestPresentationFiles
	body := generated.SearchParticipantFilesBody{
		Cursor:     optionalString(request.Cursor),
		Limit:      optionalPositiveInt64(request.Limit),
		Directions: []generated.PersonFileSearchHTTPRequestDirections{generated.PersonFileSearchHTTPRequestDirectionsFromPerson},
		Predicate: generated.ExploreHTTPRequest{
			Presentation: &presentation,
		},
		Sort: generated.FileSearchSort{Field: "occurred_at", Direction: "desc"},
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.SearchParticipantFilesResp, error) {
			return client.SearchParticipantFilesWithResponse(ctx,
				&generated.SearchParticipantFilesRequestOptions{
					PathParams: &generated.SearchParticipantFilesPath{ID: request.ParticipantID},
					Body:       &body,
				})
		})
	if err != nil {
		return nil, err
	}
	page := &peoplebrowser.FilePage{
		Rows:          make([]query.FileRow, len(resp.JSON200.Files)),
		TotalCount:    resp.JSON200.TotalCount,
		NextCursor:    stringValue(resp.JSON200.NextCursor),
		CacheRevision: resp.JSON200.CacheRevision,
	}
	for i, row := range resp.JSON200.Files {
		page.Rows[i] = fileRowFromGenerated(row)
	}
	return page, nil
}

func (b *PeopleBrowser) ListActivity(
	ctx context.Context, request peoplebrowser.ActivityPageRequest,
) (*peoplebrowser.ActivityPage, error) {
	response, err := b.participantTimeline(ctx, request, nil)
	if err != nil {
		return nil, err
	}
	page := &peoplebrowser.ActivityPage{
		Rows:          make([]query.EntryRow, len(response.Rows)),
		TotalCount:    int64Value(response.TotalCount),
		NextCursor:    stringValue(response.NextCursor),
		CacheRevision: response.CacheRevision,
	}
	for i, row := range response.Rows {
		page.Rows[i] = entryRowFromGenerated(row)
	}
	return page, nil
}

func (b *PeopleBrowser) GetMessage(
	ctx context.Context, messageID int64,
) (*query.MessageDetail, error) {
	return b.engine.GetMessage(ctx, messageID)
}

func (b *PeopleBrowser) participantTimeline(
	ctx context.Context,
	request peoplebrowser.ContactPageRequest,
	filters []generated.ExploreFilter,
) (*generated.GetParticipantTimelineResponse, error) {
	presentation := generated.ExploreHTTPRequestPresentationTable
	body := generated.GetParticipantTimelineBody{
		Cursor:       optionalString(request.Cursor),
		Limit:        optionalPositiveInt64(request.Limit),
		Filters:      filters,
		Presentation: &presentation,
		Sort: []generated.ExploreSort{{
			Field: generated.ExploreSortFieldOccurredAt, Direction: generated.ExploreSortDirectionDesc,
		}},
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.GetParticipantTimelineResp, error) {
			return client.GetParticipantTimelineWithResponse(ctx,
				&generated.GetParticipantTimelineRequestOptions{
					PathParams: &generated.GetParticipantTimelinePath{ID: request.ParticipantID},
					Body:       &body,
				})
		})
	if err != nil {
		return nil, err
	}
	return resp.JSON200, nil
}

func personSummaryFromGenerated(person generated.PersonSummary) query.PersonSummary {
	identifiers := make([]query.PersonIdentifier, len(person.Identifiers))
	for i, identifier := range person.Identifiers {
		identifiers[i] = query.PersonIdentifier{
			Type: identifier.Type, Value: identifier.Value,
			DisplayValue: stringValue(identifier.DisplayValue), IsPrimary: identifier.IsPrimary,
			Provenance: identifier.Provenance, ParticipantID: identifier.ParticipantID,
		}
	}
	counts := make([]query.SourceCount, len(person.SourceCounts))
	for i, count := range person.SourceCounts {
		counts[i] = query.SourceCount{SourceType: count.SourceType, Count: count.Count}
	}
	out := query.PersonSummary{
		ID: person.ID, DisplayLabel: person.DisplayLabel,
		DisplayName: stringValue(person.DisplayName), PartialLabel: person.PartialLabel,
		Identifiers: identifiers, ActivityCount: person.ActivityCount,
		MeetingCount: person.MeetingCount, FileCount: person.FileCount, SourceCounts: counts,
		FirstAt: person.FirstAt, LastAt: person.LastAt, CacheRevision: person.CacheRevision,
	}
	if person.Cluster != nil {
		edges := make([]query.PersonClusterEdge, len(person.Cluster.Edges))
		for i, edge := range person.Cluster.Edges {
			edges[i] = query.PersonClusterEdge{
				ParticipantA: edge.ParticipantA, ParticipantB: edge.ParticipantB,
			}
		}
		out.Cluster = &query.PersonCluster{
			CanonicalID: person.Cluster.CanonicalID,
			MemberIDs:   append([]int64(nil), person.Cluster.MemberIds...),
			Edges:       edges,
		}
	}
	if person.Profile != nil {
		out.Profile = &query.PersonProfile{
			ID: person.Profile.ID, DisplayName: copyString(person.Profile.DisplayName),
			Revision: person.Profile.Revision,
		}
	}
	return out
}

func personFromGenerated(person *generated.Person) *store.Person {
	if person == nil {
		return nil
	}
	return &store.Person{
		ID: person.ID, VCardUID: person.VcardUID,
		DisplayName: copyString(person.DisplayName), Revision: person.Revision,
		ParticipantIDs: append([]int64(nil), person.ParticipantIds...),
		CreatedAt:      person.CreatedAt, UpdatedAt: person.UpdatedAt,
	}
}

func attributeDefinitionFromGenerated(definition generated.AttributeDefinition) store.AttributeDefinition {
	out := store.AttributeDefinition{
		ID: definition.ID, UniversalID: definition.UniversalID,
		ObjectType: store.AttributeObjectType(definition.ObjectType), Slug: definition.Slug,
		Label: definition.Label, Description: copyString(definition.Description),
		ValueType:    store.AttributeValueType(definition.ValueType),
		FieldType:    store.AttributeFieldType(definition.FieldType),
		RecordTarget: copyString(definition.RecordTarget),
		Cardinality:  store.AttributeCardinality(definition.Cardinality),
		DisplayOrder: definition.DisplayOrder, IsRequired: definition.IsRequired,
		Ownership:   store.AttributeOwnership(definition.Ownership),
		UICreatable: definition.UICreatable, UIEditable: definition.UIEditable,
		APIMutable: definition.APIMutable, IsSearchable: definition.IsSearchable,
		IsSensitive: definition.IsSensitive, IsAudited: definition.IsAudited,
		IsDeletable: definition.IsDeletable, HistoryExempt: definition.HistoryExempt,
		DerivedSource: copyString(definition.DerivedSource),
		VCardProperty: copyString(definition.VcardProperty), IsActive: definition.IsActive,
		Revision: definition.Revision, CreatedAt: definition.CreatedAt, UpdatedAt: definition.UpdatedAt,
	}
	if definition.Options != nil {
		choices := make([]store.AttributeChoice, len(definition.Options.Choices))
		for i, choice := range definition.Options.Choices {
			choices[i] = store.AttributeChoice{Value: choice.Value, Label: choice.Label}
		}
		out.Options = &store.AttributeOptions{
			Choices: choices, Unit: stringValue(definition.Options.Unit),
			MaxLength: int(int64Value(definition.Options.MaxLength)),
		}
	}
	return out
}

func attributeValueFromGenerated(value generated.AttributeValue) store.AttributeValue {
	return store.AttributeValue{
		Type: store.AttributeValueType(value.Type), Text: copyString(value.Text),
		Integer: copyInt64(value.Integer), Real: copyFloat64(value.Real),
		Boolean: copyBool(value.Boolean), Date: copyString(value.Date),
		Timestamp: copyTime(value.Timestamp), JSON: append(json.RawMessage(nil), value.JSON...),
		RecordType: copyString(value.RecordType), RecordID: copyInt64(value.RecordID),
	}
}

func attributeValueToGenerated(value store.AttributeValue) generated.AttributeValue {
	return generated.AttributeValue{
		Type: string(value.Type), Text: copyString(value.Text), Integer: copyInt64(value.Integer),
		Real: copyFloat64(value.Real), Boolean: copyBool(value.Boolean), Date: copyString(value.Date),
		Timestamp: copyTime(value.Timestamp), JSON: append(json.RawMessage(nil), value.JSON...),
		RecordType: copyString(value.RecordType), RecordID: copyInt64(value.RecordID),
	}
}

func personAttributeValueFromGenerated(value generated.PersonAttributeValue) store.PersonAttributeValue {
	return store.PersonAttributeValue{
		ID: value.ID, PersonID: value.PersonID, DefinitionID: value.DefinitionID,
		DefinitionSlug: value.DefinitionSlug, Ordinal: value.Ordinal,
		Value: attributeValueFromGenerated(value.Value), ActiveFrom: value.ActiveFrom,
		ActiveUntil: copyTime(value.ActiveUntil), CreatedAt: value.CreatedAt,
		SupersededAt: copyTime(value.SupersededAt), Source: store.Provenance(value.Source),
		SourceRef: copyString(value.SourceRef), Confidence: copyFloat64(value.Confidence),
		Actor: copyString(value.Actor),
	}
}

func personAttributeWriteFromGenerated(write *generated.PersonAttributeWrite) store.PersonAttributeWrite {
	if write == nil {
		return store.PersonAttributeWrite{}
	}
	out := store.PersonAttributeWrite{DryRun: write.DryRun}
	if write.Value != nil {
		value := personAttributeValueFromGenerated(*write.Value)
		out.Value = &value
	}
	if write.Superseded != nil {
		value := personAttributeValueFromGenerated(*write.Superseded)
		out.Superseded = &value
	}
	return out
}

func meetingSummaryFromGenerated(row generated.EntryRow) query.MessageSummary {
	return query.MessageSummary{
		ID: *row.AnchorMessageID, SourceID: row.SourceID,
		ConversationID: int64Value(row.ConversationID), Subject: row.Title,
		Snippet: row.Preview, SentAt: row.OccurredAt,
		HasAttachments: row.HasAttachments, AttachmentCount: int(row.AttachmentCount),
		MessageType: row.MessageType, ConversationTitle: row.Title,
	}
}

func fileRowFromGenerated(row generated.PersonFileSearchRow) query.FileRow {
	provenance := &query.PersonFileProvenance{
		ParticipantIDs: append([]int64(nil), row.PersonProvenance.ParticipantIds...),
		Roles:          make([]query.PersonFileRole, len(row.PersonProvenance.Roles)),
		Directions:     make([]query.PersonFileDirection, len(row.PersonProvenance.Directions)),
	}
	for i, role := range row.PersonProvenance.Roles {
		provenance.Roles[i] = query.PersonFileRole(role)
	}
	for i, direction := range row.PersonProvenance.Directions {
		provenance.Directions[i] = query.PersonFileDirection(direction)
	}
	return query.FileRow{
		ID: row.ID, Key: row.Key, EntryKey: row.EntryKey,
		MessageID: row.MessageID, ConversationID: row.ConversationID,
		OccurredAt: row.OccurredAt, SourceID: row.SourceID,
		SourceType: row.SourceType, SourceIdentifier: row.SourceIdentifier,
		ContainingTitle: row.ContainingTitle, Filename: stringValue(row.Filename),
		MimeType: stringValue(row.MimeType), MIMEFamily: query.FileMIMEFamily(row.MimeFamily),
		Size: row.SizeBytes, ParticipantIDs: append([]int64(nil), row.ParticipantIds...),
		ParticipantLabels:  append([]string(nil), row.ParticipantLabels...),
		ParticipantDomains: append([]string(nil), row.ParticipantDomains...),
		PersonProvenance:   provenance,
	}
}

func entryRowFromGenerated(row generated.EntryRow) query.EntryRow {
	return query.EntryRow{
		Key: row.Key, Kind: query.EntryKind(row.Kind), AnchorMessageID: copyInt64(row.AnchorMessageID),
		ConversationID: copyInt64(row.ConversationID), OccurredAt: row.OccurredAt,
		Match: query.MatchSummary{
			LexicalMatchCount: copyInt64(row.Match.LexicalMatchCount),
			StrongestExcerpt:  stringValue(row.Match.StrongestExcerpt),
			SemanticScore:     copyFloat64(row.Match.SemanticScore),
		},
		SourceID: row.SourceID, SourceType: row.SourceType,
		SourceIdentifier: row.SourceIdentifier, MessageType: row.MessageType,
		ConversationType: row.ConversationType, Title: row.Title, Preview: row.Preview,
		ParticipantIDs:             append([]int64(nil), row.ParticipantIds...),
		ParticipantLabels:          append([]string(nil), row.ParticipantLabels...),
		MatchedSenderIdentities:    append([]string(nil), row.MatchedSenderIdentities...),
		MatchedRecipientIdentities: append([]string(nil), row.MatchedRecipientIdentities...),
		MessageCount:               row.MessageCount, HasAttachments: row.HasAttachments,
		AttachmentCount: row.AttachmentCount, AttachmentSize: row.AttachmentSize,
		DeletedFromSource:        row.DeletedFromSource,
		CounterpartParticipantID: copyInt64(row.CounterpartParticipantID),
	}
}

func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func copyBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
