package accountops

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
)

// SettingsStore keeps maintenance capabilities out of read-only adapters.
type SettingsStore interface {
	Store
	UpdateSourceSettingsContext(ctx context.Context, sourceID int64, update store.SourceSettingsUpdate) (store.SourceSettings, error)
}

type MergeStore interface {
	sourceops.Store
	MergeSourcesContext(ctx context.Context, req store.MergeSourcesRequest) (store.SourceMergeResult, error)
}

// MergeRequest requires an exact selector for each endpoint.
type MergeRequest struct {
	From            string `json:"from,omitempty"`
	Into            string `json:"into,omitempty"`
	FromSourceID    int64  `json:"from_source_id,omitzero"`
	IntoSourceID    int64  `json:"into_source_id,omitzero"`
	FromSourceIDSet bool   `json:"-"`
	IntoSourceIDSet bool   `json:"-"`
	DryRun          bool   `json:"dry_run"`
	DryRunSet       bool   `json:"-"`
}

func (r *MergeRequest) UnmarshalJSON(data []byte) error {
	type request MergeRequest
	var decoded request
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, decoded.FromSourceIDSet = fields["from_source_id"]
	_, decoded.IntoSourceIDSet = fields["into_source_id"]
	decoded.DryRunSet = string(fields["dry_run"]) == "true" || string(fields["dry_run"]) == "false"
	*r = MergeRequest(decoded)
	return nil
}

func UpdateAccount(ctx context.Context, st SettingsStore, req UpdateRequest) (UpdateResult, error) {
	account := strings.TrimSpace(req.Account)
	email := strings.TrimSpace(req.Email)
	if account != "" && email != "" && account != email {
		return UpdateResult{}, opserr.Invalid(errors.New("account and email selectors are mutually exclusive"))
	}
	if account == "" {
		account = email
	}
	source, err := sourceops.ResolveExactOneContext(ctx, st, sourceops.Selector{Account: account, SourceID: req.SourceID, SourceIDSet: req.SourceIDSet})
	if err != nil {
		return UpdateResult{}, err
	}
	update := store.SourceSettingsUpdate{Alias: req.Identifier, HistoryOnly: req.HistoryOnly, AcceptReanchor: req.AcceptReanchor}
	if req.DisplayName != "" {
		update.DisplayName = &req.DisplayName
	}
	settings, err := st.UpdateSourceSettingsContext(ctx, source.ID, update)
	if err != nil {
		return UpdateResult{}, maintenanceError(err)
	}
	displayName := source.DisplayName.String
	if update.DisplayName != nil {
		displayName = *update.DisplayName
	}
	return UpdateResult{Email: source.Identifier, Identifier: source.Identifier, DisplayName: displayName, Alias: settings.Alias, HistoryOnly: settings.HistoryOnly, ReanchorRequired: settings.ReanchorRequired}, nil
}

func MergeAccounts(ctx context.Context, st MergeStore, req MergeRequest) (store.SourceMergeResult, error) {
	if !req.DryRunSet {
		return store.SourceMergeResult{}, opserr.Invalid(errors.New("dry_run must be explicitly true or false"))
	}
	from, err := sourceops.ResolveExactOneContext(ctx, st, sourceops.Selector{Account: req.From, SourceID: req.FromSourceID, SourceIDSet: req.FromSourceIDSet})
	if err != nil {
		return store.SourceMergeResult{}, err
	}
	into, err := sourceops.ResolveExactOneContext(ctx, st, sourceops.Selector{Account: req.Into, SourceID: req.IntoSourceID, SourceIDSet: req.IntoSourceIDSet})
	if err != nil {
		return store.SourceMergeResult{}, err
	}
	result, err := st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: req.DryRun})
	if err != nil {
		return result, maintenanceError(err)
	}
	return result, nil
}

func maintenanceError(err error) error {
	if errors.Is(err, store.ErrSourceSettingsInvalid) || errors.Is(err, store.ErrSourceRetired) || errors.Is(err, store.ErrSourceMergeInvalid) || errors.Is(err, store.ErrSyncAlreadyActive) {
		return opserr.Invalid(err)
	}
	if errors.Is(err, store.ErrSourceNotFound) {
		return opserr.NotFound(err)
	}
	return opserr.Internal(fmt.Errorf("source maintenance: %w", err))
}
