package sync

import (
	"context"
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

// Acquisition diagnostic states.
const (
	AcquisitionActive        = "active"
	AcquisitionResolved      = "resolved"
	AcquisitionHistorical    = "historical"
	AcquisitionIndeterminate = "indeterminate"
)

// AcquisitionDiagnosticItem is the newest recorded failure for a message in
// one state.
type AcquisitionDiagnosticItem struct {
	SourceMessageID string `json:"source_message_id"`
	Phase           string `json:"phase"`
	ErrorKind       string `json:"error_kind"`
	State           string `json:"state"`
}

type acquisitionOperation int

const (
	acquisitionUnknown acquisitionOperation = iota
	acquisitionDownload
	acquisitionFiles
)

func acquisitionOperationOf(item store.SyncRunItem) acquisitionOperation {
	switch {
	case !isAcquisitionReplayCandidate(item):
		return acquisitionUnknown
	case materializationFromKind(item.ErrorKind) != 0:
		return acquisitionFiles
	default:
		return acquisitionDownload
	}
}

// DiagnoseAcquisitionContext classifies a source's recorded fetch and ingest
// failures in one read snapshot. Active failures are the ones sync retries.
// Resolved failures have an explicit resolution. Historical failures were
// skipped by cleanup. The remaining failures are indeterminate.
func DiagnoseAcquisitionContext(ctx context.Context, st *store.Store, sourceID int64) ([]AcquisitionDiagnosticItem, error) {
	ctx, release, err := st.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if source.SourceType != "" && source.SourceType != sourceTypeGmail {
		return nil, errors.New("acquisition diagnostic requires a Gmail source")
	}
	candidates, err := st.ListUnresolvedSyncItemErrors(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	outcomes, err := st.ListAcquisitionOutcomesContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}

	retries := make(map[string]store.SyncRunItem, len(candidates))
	for _, item := range candidates {
		retries[item.SourceMessageID] = item
	}
	resolvedLater := make(map[string]bool)
	representatives := make(map[[2]string]bool)
	items := make([]AcquisitionDiagnosticItem, 0)
	// Outcomes arrive newest first, so resolutions are always later.
	for _, outcome := range outcomes {
		resolved := resolvedLater[outcome.SourceMessageID]
		op := acquisitionOperationOf(outcome)
		var state string
		switch {
		// ListSyncRunItemsContext filters these markers in SQL before its limit;
		// this source-wide walk already has the later resolution in hand.
		case outcome.ErrorMessage == store.SyncRunItemFilesPendingMessage && materializationFromKind(outcome.ErrorKind) != 0 && resolved:
		case outcome.Status != store.SyncRunItemStatusError && outcome.Status != store.SyncRunItemStatusSkipped:
		case outcome.Status == store.SyncRunItemStatusSkipped && op != acquisitionUnknown && !resolved:
			state = AcquisitionHistorical
		case op != acquisitionUnknown && resolved:
			state = AcquisitionResolved
		case outcome.Status == store.SyncRunItemStatusSkipped:
		case source.MergedIntoSourceID == 0 && op != acquisitionUnknown && acquisitionOperationOf(retries[outcome.SourceMessageID]) == op:
			state = AcquisitionActive
		default:
			state = AcquisitionIndeterminate
		}
		if outcome.Status == store.SyncRunItemStatusResolved {
			resolvedLater[outcome.SourceMessageID] = true
		}
		key := [2]string{outcome.SourceMessageID, state}
		if state == "" || representatives[key] {
			continue
		}
		representatives[key] = true
		items = append(items, AcquisitionDiagnosticItem{outcome.SourceMessageID, outcome.Phase, outcome.ErrorKind, state})
	}
	slices.Reverse(items)
	return items, nil
}
