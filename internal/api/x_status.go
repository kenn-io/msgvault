package api

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/x"
)

// XSourceStatus is local metadata only. Tokens, raw control state, provider
// errors, message content and opaque pagination/query data never enter it.
type XSourceStatus struct {
	Generation          int64            `json:"credential_generation"`
	MonthlyBudgetMicros int64            `json:"monthly_budget_micros"`
	OwnComplete         bool             `json:"own_complete"`
	MentionsComplete    bool             `json:"mentions_complete"`
	HistoryGapPossible  bool             `json:"history_gap_possible"`
	CredentialError     string           `json:"credential_error,omitempty"`
	ModelError          string           `json:"model_error,omitempty"`
	Month               string           `json:"month"`
	EstimatedMicros     int64            `json:"month_estimated_micros"`
	ReservedMicros      int64            `json:"month_reserved_micros"`
	Retention           x.RefreshSummary `json:"retention"`
}

// XSourceStatusReader is optional so existing embedded hosts keep their narrow
// Store interface. The daemon supplies its actual policy and explicit clock.
type XSourceStatusReader interface {
	ReadXSourceStatusContext(context.Context, int64, time.Time) (*XSourceStatus, error)
}

func (s *Server) addXSourceStatus(ctx context.Context, source *store.Source, status *SourceStatus) error {
	if source.SourceType != "x" {
		return nil
	}
	// The status route must stay usable while content reads are fenced. Even
	// old run diagnostics or editable source labels must not leak removed text.
	status.DisplayName = nil
	if status.SchedulerLastError != "" {
		status.SchedulerLastError = "X sync failed; inspect credential, budget and retention status"
	}
	for _, run := range []*SyncRunStatus{status.ActiveSync, status.LatestSync, status.LastSuccessfulSync} {
		if run != nil {
			if run.ErrorMessage != nil && *run.ErrorMessage != "" {
				run.ErrorMessage = new("X sync failed; inspect credential, budget and retention status")
			}
			run.ItemErrors = nil
		}
	}
	if reader, ok := s.store.(XSourceStatusReader); ok {
		var err error
		status.X, err = reader.ReadXSourceStatusContext(ctx, source.ID, s.clockNow())
		return err
	}
	return nil
}
