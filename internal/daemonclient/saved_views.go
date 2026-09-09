package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/savedview"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) ListSavedViews(ctx context.Context) ([]store.SavedView, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListSavedViewsResp, error) {
		return client.ListSavedViewsWithResponse(ctx)
	})
	if err != nil {
		return nil, savedViewAPIError(err)
	}

	views := make([]store.SavedView, 0, len(resp.JSON200.SavedViews))
	for i := range resp.JSON200.SavedViews {
		view, err := savedViewFromGenerated(resp.JSON200.SavedViews[i])
		if err != nil {
			return nil, err
		}
		views = append(views, *view)
	}
	return views, nil
}

func (c *Client) GetSavedView(ctx context.Context, id int64) (*store.SavedView, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetSavedViewResp, error) {
		return client.GetSavedViewWithResponse(ctx, &generated.GetSavedViewRequestOptions{
			PathParams: &generated.GetSavedViewPath{ID: id},
		})
	})
	if err != nil {
		return nil, savedViewAPIError(err)
	}
	return savedViewFromGenerated(*resp.JSON200)
}

func (c *Client) CreateSavedView(
	ctx context.Context,
	input store.SavedViewInput,
) (*store.SavedView, error) {
	state, err := savedViewStateFromJSON(input.CanonicalState)
	if err != nil {
		return nil, err
	}
	resp, err := APIResponseWithStatuses(c, []int{http.StatusCreated}, func(client *apiclient.Client) (*generated.CreateSavedViewResp, error) {
		return client.CreateSavedViewWithResponse(ctx, &generated.CreateSavedViewRequestOptions{
			Body: &generated.CreateSavedViewRequest{
				Name: input.Name, Description: input.Description, CanonicalState: state,
				SchemaVersion: int64(input.SchemaVersion),
			},
		})
	})
	if err != nil {
		return nil, savedViewAPIError(err)
	}
	return savedViewFromGenerated(*resp.JSON201)
}

func (c *Client) UpdateSavedView(
	ctx context.Context,
	id, expectedRevision int64,
	patch savedview.Patch,
) (*store.SavedView, error) {
	body := generated.PatchSavedViewRequest{
		Name: patch.Name, Description: patch.Description,
	}
	if patch.CanonicalState != nil {
		state, err := savedViewStateFromEnvelope(*patch.CanonicalState)
		if err != nil {
			return nil, err
		}
		body.CanonicalState = &state
	}
	if patch.SchemaVersion != nil {
		value := int64(*patch.SchemaVersion)
		body.SchemaVersion = &value
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.PatchSavedViewResp, error) {
		return client.PatchSavedViewWithResponse(ctx, &generated.PatchSavedViewRequestOptions{
			PathParams: &generated.PatchSavedViewPath{ID: id},
			Header: &generated.PatchSavedViewHeaders{
				IfMatch: savedViewRevisionTag(id, expectedRevision),
			},
			Body: &body,
		})
	})
	if err != nil {
		return nil, savedViewAPIError(err)
	}
	return savedViewFromGenerated(*resp.JSON200)
}

func (c *Client) DeleteSavedView(ctx context.Context, id, expectedRevision int64) error {
	_, err := APIResponseWithStatuses(c, []int{http.StatusNoContent}, func(client *apiclient.Client) (*generated.DeleteSavedViewResp, error) {
		return client.DeleteSavedViewWithResponse(ctx, &generated.DeleteSavedViewRequestOptions{
			PathParams: &generated.DeleteSavedViewPath{ID: id},
			Header: &generated.DeleteSavedViewHeaders{
				IfMatch: savedViewRevisionTag(id, expectedRevision),
			},
		})
	})
	return savedViewAPIError(err)
}

// RunSavedView asks the daemon to execute the view through its canonical
// Explore definition. The daemon owns the translation, so a view runs the
// same way here as when the Web UI opens it.
func (c *Client) RunSavedView(
	ctx context.Context,
	id int64,
	limit int,
	cursor string,
) (*savedview.RunPage, error) {
	body := generated.RunSavedViewRequest{Limit: new(int64(limit))}
	if cursor != "" {
		body.Cursor = &cursor
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.RunSavedViewResp, error) {
		return client.RunSavedViewWithResponse(ctx, &generated.RunSavedViewRequestOptions{
			PathParams: &generated.RunSavedViewPath{ID: id},
			Body:       &body,
		})
	})
	if err != nil {
		return nil, savedViewAPIError(err)
	}
	return runPageFromGenerated(*resp.JSON200)
}

func runPageFromGenerated(result generated.RunSavedViewResponse) (*savedview.RunPage, error) {
	view, err := savedViewFromGenerated(result.SavedView)
	if err != nil {
		return nil, err
	}
	page := &savedview.RunPage{
		View: *view, ResultKind: savedview.ResultKind(result.ResultKind),
		TotalCount: result.TotalCount, NextCursor: stringValue(result.NextCursor),
		CacheRevision: result.CacheRevision,
		SearchProvenance: query.SearchProvenance{
			LexicalIndexRevision: stringValue(result.SearchProvenance.LexicalIndexRevision),
			VectorGeneration:     result.SearchProvenance.VectorGeneration,
		},
		CandidateSnapshotID:    stringValue(result.CandidateSnapshotID),
		CandidatePoolSaturated: boolValue(result.CandidatePoolSaturated),
		SearchDeletionScope:    stringValue(result.SearchDeletionScope),
	}
	switch page.ResultKind {
	case savedview.ResultEntries:
		page.Rows = make([]query.EntryRow, len(result.Rows))
		for i := range result.Rows {
			page.Rows[i] = entryRowFromGenerated(result.Rows[i])
		}
	case savedview.ResultGroups:
		page.Groups = make([]query.ExploreGroupRow, len(result.Groups))
		for i, group := range result.Groups {
			page.Groups[i] = query.ExploreGroupRow{
				Key: group.Key, Label: group.Label, Count: group.Count,
				EstimatedBytes: group.EstimatedBytes, LatestAt: group.LatestAt,
			}
		}
	case savedview.ResultFiles:
		page.Files = make([]query.ExploreFileFact, len(result.Files))
		for i, file := range result.Files {
			page.Files[i] = query.ExploreFileFact{
				ID: file.ID, Key: file.Key, EntryKey: file.EntryKey, MessageID: file.MessageID,
				ConversationID: file.ConversationID, OccurredAt: file.OccurredAt,
				SourceID: file.SourceID, SourceIdentifier: file.SourceIdentifier,
				Title: file.Title, Filename: file.Filename, MimeType: file.MimeType, Size: file.Size,
			}
		}
	default:
		return nil, fmt.Errorf("saved view %d run returned unknown result kind %q", view.ID, result.ResultKind)
	}
	return page, nil
}

func savedViewFromGenerated(view generated.SavedView) (*store.SavedView, error) {
	state, err := json.Marshal(view.CanonicalState)
	if err != nil {
		return nil, fmt.Errorf("encode Saved View %d canonical state: %w", view.ID, err)
	}
	var reason string
	if view.IncompatibilityReason != nil {
		reason = *view.IncompatibilityReason
	}
	return &store.SavedView{
		IncompatibilityReason: reason,
		ID:                    view.ID, Name: view.Name, Description: view.Description,
		CanonicalState: state, SchemaVersion: int(view.SchemaVersion), Revision: view.Revision,
		CreatedAt: view.CreatedAt, UpdatedAt: view.UpdatedAt,
	}, nil
}

func savedViewStateFromJSON(data []byte) (generated.SavedViewStateEnvelope, error) {
	// Decoding into the typed request body erases the difference between an
	// absent field and an explicit null, which the store rejects, so the state
	// has to be checked while it is still raw.
	if err := store.ValidateSavedViewStateJSON(data); err != nil {
		return generated.SavedViewStateEnvelope{}, err
	}
	var state generated.SavedViewStateEnvelope
	if err := json.Unmarshal(data, &state); err != nil {
		return generated.SavedViewStateEnvelope{}, fmt.Errorf("decode Saved View canonical state: %w", err)
	}
	return state, nil
}

func savedViewStateFromEnvelope(state store.SavedViewStateEnvelope) (generated.SavedViewStateEnvelope, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return generated.SavedViewStateEnvelope{}, fmt.Errorf("encode Saved View canonical state: %w", err)
	}
	return savedViewStateFromJSON(data)
}

func savedViewRevisionTag(id, revision int64) string {
	return fmt.Sprintf(`"saved-view-%d-r%d"`, id, revision)
}

func savedViewAPIError(err error) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "saved_view_not_found":
		return fmt.Errorf("%w: %s", store.ErrSavedViewNotFound, apiErr.Message)
	case "saved_view_name_conflict":
		return fmt.Errorf("%w: %s", store.ErrSavedViewNameConflict, apiErr.Message)
	case "saved_view_revision_conflict":
		return fmt.Errorf("%w: %s", store.ErrSavedViewRevisionConflict, apiErr.Message)
	case "invalid_saved_view":
		return fmt.Errorf("%w: %s", store.ErrSavedViewInvalidState, apiErr.Message)
	default:
		return err
	}
}
