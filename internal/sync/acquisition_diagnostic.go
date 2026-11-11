package sync

import (
	"cmp"
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
	id              int64
}

type acquisitionOperation int

const (
	acquisitionUnknown acquisitionOperation = iota
	acquisitionDownload
	acquisitionFiles
)

func acquisitionOperationOf(item store.SyncRunItem) acquisitionOperation {
	switch {
	case !IsAcquisitionReplayCandidate(item):
		return acquisitionUnknown
	case materializationFromKind(item.ErrorKind) != 0:
		return acquisitionFiles
	default:
		return acquisitionDownload
	}
}

// DiagnoseAcquisitionContext classifies a source's recorded fetch and ingest
// failures in one read snapshot. Active failures are the ones sync retries.
// Resolved failures have a later explicit resolution. Historical failures were
// skipped, or are downloads followed by a later skip; file-saving work needs
// explicit settlement. The remaining failures are indeterminate.
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
		case outcome.Status == store.SyncRunItemStatusSkipped && op != acquisitionUnknown:
			state = AcquisitionHistorical
		case outcome.Status != store.SyncRunItemStatusError:
		case op != acquisitionUnknown && next.resolved:
			if outcome.ErrorMessage != store.SyncRunItemFilesPendingMessage {
				state = AcquisitionResolved
			}
		case op == acquisitionDownload && next.skipped:
			state = AcquisitionHistorical
		case op != acquisitionUnknown && covers(retries[outcome.SourceMessageID], outcome, op):
			state = AcquisitionActive
		default:
			state = AcquisitionIndeterminate
		}
		switch outcome.Status {
		case store.SyncRunItemStatusResolved:
			next.resolved = true
		case store.SyncRunItemStatusSkipped:
			next.skipped = true
		}
		seen[outcome.SourceMessageID] = next
		key := [2]string{outcome.SourceMessageID, state}
		if state == "" || representatives[key] {
			continue
		}
		representatives[key] = true
		items = append(items, AcquisitionDiagnosticItem{outcome.SourceMessageID, outcome.Phase, outcome.ErrorKind, state, outcome.ID})
	}
	slices.SortFunc(items, func(a, b AcquisitionDiagnosticItem) int { return cmp.Compare(a.id, b.id) })
	return items, nil
}

// covers reports whether the retry candidate stands for an error of the same
// operation recorded no later than the candidate.
func covers(retry, outcome store.SyncRunItem, op acquisitionOperation) bool {
	return retry.ID >= outcome.ID && acquisitionOperationOf(retry) == op
}
