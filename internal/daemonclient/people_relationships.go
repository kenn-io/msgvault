package daemonclient

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// RelationshipCadenceMinAPISchemaVersion adds cadence filters to the
// relationship ranking. An older daemon would ignore them and return the
// unfiltered ranking.
const RelationshipCadenceMinAPISchemaVersion = "3.44.0"

var _ peoplebrowser.RelationshipLister = (*PeopleBrowser)(nil)

func (b *PeopleBrowser) ListRelationships(
	ctx context.Context, request peoplebrowser.RelationshipListRequest,
) (*peoplebrowser.RelationshipListPage, error) {
	if request.Cadence != "" || request.DueWithinDays != nil {
		supported, err := b.engine.store.SupportsAPISchemaVersion(ctx, RelationshipCadenceMinAPISchemaVersion)
		if err != nil {
			return nil, fmt.Errorf("check relationship cadence capability: %w", err)
		}
		if !supported {
			return nil, fmt.Errorf("relationship cadence requires daemon API schema %s or newer; upgrade the daemon", RelationshipCadenceMinAPISchemaVersion)
		}
	}
	options := &generated.ListRelationshipsRequestOptions{
		Body: &generated.ListRelationshipsBody{
			ShowAll:       boolPointer(request.ShowAll),
			Cursor:        optionalString(request.Cursor),
			Limit:         optionalPositiveInt64(request.Limit),
			Cadence:       relationshipCadencePointer(request.Cadence),
			DueWithinDays: int64Pointer(request.DueWithinDays),
		},
	}
	resp, err := APIResponse(b.engine.store,
		func(client *apiclient.Client) (*generated.ListRelationshipsResp, error) {
			return client.ListRelationshipsWithResponse(ctx, options)
		})
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("empty relationships response")
	}
	page := &peoplebrowser.RelationshipListPage{
		Rows:             make([]query.RelationshipRow, len(resp.JSON200.Rows)),
		TotalCount:       resp.JSON200.TotalCount,
		CacheRevision:    resp.JSON200.CacheRevision,
		IdentityRevision: resp.JSON200.IdentityRevision,
		NextCursor:       stringValue(resp.JSON200.NextCursor),
	}
	for i, row := range resp.JSON200.Rows {
		page.Rows[i] = relationshipRowFromGenerated(row)
	}
	return page, nil
}

func relationshipRowFromGenerated(row generated.RelationshipRow) query.RelationshipRow {
	result := query.RelationshipRow{
		CanonicalID:  row.CanonicalID,
		DisplayLabel: row.DisplayLabel,
		MemberIDs:    append([]int64(nil), row.MemberIds...),
		Score:        row.Score,
		LastAt:       row.LastAt,
		Signals: query.RelationshipSignals{
			SentToThem:        row.Signals.SentToThem,
			ReceivedFromThem:  row.Signals.ReceivedFromThem,
			MeetingsTogether:  row.Signals.MeetingsTogether,
			SentCount:         row.Signals.SentCount,
			MeetingCount:      row.Signals.MeetingCount,
			Modalities:        int(row.Signals.Modalities),
			LastInteractionAt: row.Signals.LastInteractionAt,
		},
	}
	if row.PersonID != nil {
		personID := *row.PersonID
		result.PersonID = &personID
	}
	if row.Cadence != nil {
		result.Cadence = &query.RelationshipCadence{
			FrequencyDays: row.Cadence.FrequencyDays,
			List:          stringValue(row.Cadence.List),
			DueAt:         row.Cadence.DueAt,
			Status:        row.Cadence.Status,
			DaysOverdue:   intPointer(row.Cadence.DaysOverdue),
			Stale:         row.Cadence.Stale,
		}
	}
	result.LastContactChannel = stringValue(row.LastContactChannel)
	return result
}

func boolPointer(value bool) *bool {
	return &value
}

func int64Pointer(value *int) *int64 {
	if value == nil {
		return nil
	}
	n := int64(*value)
	return &n
}

func intPointer(value *int64) *int {
	if value == nil {
		return nil
	}
	n := int(*value)
	return &n
}

func relationshipCadencePointer(value string) *generated.RelationshipsHTTPRequestCadence {
	if value == "" {
		return nil
	}
	cadence := generated.RelationshipsHTTPRequestCadence(value)
	return &cadence
}
