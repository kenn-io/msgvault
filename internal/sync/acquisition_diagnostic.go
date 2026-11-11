package sync

import (
	"context"
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
// skipped, or are downloads whose next settlement is a skip; file-saving work
// needs explicit settlement. The remaining failures are indeterminate.
func DiagnoseAcquisitionContext(ctx context.Context, st *store.Store, sourceID int64) ([]AcquisitionDiagnosticItem, error) {
	ctx, release, err := st.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
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
	type later struct{ resolved, skipped bool }
	seen := make(map[string]later)
	representatives := make(map[[2]string]bool)
	items := make([]AcquisitionDiagnosticItem, 0)
	// Outcomes arrive newest first, so seen holds only later outcomes.
	for _, outcome := range outcomes {
		next := seen[outcome.SourceMessageID]
		op := acquisitionOperationOf(outcome)
		var state string
		switch {
		case outcome.ErrorMessage == store.SyncRunItemFilesPendingMessage && materializationFromKind(outcome.ErrorKind) != 0 && next.resolved:
		case outcome.Status != store.SyncRunItemStatusError && outcome.Status != store.SyncRunItemStatusSkipped:
		case op == acquisitionDownload && next.skipped:
			state = AcquisitionHistorical
		case op != acquisitionUnknown && next.resolved:
			state = AcquisitionResolved
		case outcome.Status == store.SyncRunItemStatusSkipped && op != acquisitionUnknown:
			state = AcquisitionHistorical
		case outcome.Status == store.SyncRunItemStatusSkipped:
		case op != acquisitionUnknown && covers(retries[outcome.SourceMessageID], outcome, op):
			state = AcquisitionActive
		default:
			state = AcquisitionIndeterminate
		}
		switch outcome.Status {
		case store.SyncRunItemStatusResolved:
			next.resolved, next.skipped = true, false
		case store.SyncRunItemStatusSkipped:
			// Cleanup changes failures to skipped; only a separate skip settles downloads.
			if op == acquisitionUnknown {
				next.skipped = true
			}
		}
		seen[outcome.SourceMessageID] = next
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

// covers reports whether the retry candidate stands for an error of the same
// operation recorded no later than the candidate.
func covers(retry, outcome store.SyncRunItem, op acquisitionOperation) bool {
	return retry.ID >= outcome.ID && acquisitionOperationOf(retry) == op
}
