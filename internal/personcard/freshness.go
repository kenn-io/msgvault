package personcard

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/freshness"
)

const StaleAfterSeconds int64 = 86400

// SourceInput is the bounded source snapshot used by a card. Sync commands are
// built locally; callers cannot supply an executable command.
type SourceInput struct {
	SourceID              int64
	SourceType            string
	Identifier            string
	Scheduling            string
	LastSuccessfulSyncAt  *time.Time
	Outcome               string
	LastSyncErrors        int64
	CanSync               bool
	SyncUnavailableReason string
}
type FreshnessInputs struct {
	CollectedAt         time.Time
	Sources             []SourceInput
	SourcesOmitted      int
	OmittedStale        bool
	Unavailable         bool
	ContactStateStale   bool
	ActivityComputedAt  *time.Time
	OpenLoopsComputedAt *time.Time
	BriefComputedAt     *time.Time
}

// SourceFreshness adds card-specific sync guidance to the shared source report.
type SourceFreshness struct {
	freshness.SourceFreshness

	CanSync               bool   `json:"can_sync"`
	SyncUnavailableReason string `json:"sync_unavailable_reason"`
	IdentifierMore        bool   `json:"identifier_more,omitempty"`
}

// FreshnessBlock retains the shared report and card-specific coverage metadata.
type FreshnessBlock struct {
	freshness.Report

	Sources        []SourceFreshness `json:"sources"`
	CollectedAt    time.Time         `json:"collected_at"`
	SourcesOmitted int               `json:"sources_omitted"`
	WarningsMore   bool              `json:"warnings_more,omitempty"`
}

func buildFreshness(in FreshnessInputs) FreshnessBlock {
	sources := make([]freshness.SourceInventoryItem, 0, min(10, len(in.Sources)))
	for _, s := range in.Sources[:min(10, len(in.Sources))] {
		id := s.SourceID
		identifier, _ := clean(s.Identifier, 120)
		outcome := enum(s.Outcome, "running", "completed", "failed", "cancelled", "paused", "never", "unmeasured", "unknown")
		if outcome == "" && s.LastSuccessfulSyncAt != nil {
			// A successful archive frontier does not identify the latest attempt.
			outcome = "unknown"
		}
		sources = append(sources, freshness.SourceInventoryItem{SourceID: &id, SourceType: s.SourceType, Identifier: identifier, Scheduling: enum(s.Scheduling, "manual", "scheduled"), LastSuccessfulSyncAt: s.LastSuccessfulSyncAt, Outcome: outcome, LastSyncErrors: s.LastSyncErrors, SyncCommand: syncCommand(s)})
	}
	projections := map[string]freshness.ProjectionState{}
	if in.ActivityComputedAt != nil {
		projections["activity"] = freshness.ProjectionState{ComputedAt: in.ActivityComputedAt}
	}
	// Build cannot fail: the pure adapter supplies a nonzero clock and threshold.
	report, _ := freshness.Build(context.Background(), freshness.Inputs{Sources: sources, Projections: projections, StaleAfter: time.Duration(StaleAfterSeconds) * time.Second, Now: in.CollectedAt})
	if report.Sources == nil {
		report.Sources = []freshness.SourceFreshness{}
	}
	if report.Projections == nil {
		report.Projections = map[string]*freshness.ProjectionFreshness{}
	}
	// The shared reporter sorts inventories. Restore the person's newest-activity
	// order, which is the card's specified coverage and truncation order.
	ordered := make([]freshness.SourceFreshness, 0, len(sources))
	for _, s := range sources {
		for _, v := range report.Sources {
			if v.SourceID != nil && s.SourceID != nil && *v.SourceID == *s.SourceID {
				ordered = append(ordered, v)
				break
			}
		}
	}
	report.Sources = ordered
	for name, at := range map[string]*time.Time{"open_loops": in.OpenLoopsComputedAt, "brief": in.BriefComputedAt} {
		if at == nil {
			report.Projections[name] = nil
		} else {
			age := max(int64(0), int64(in.CollectedAt.Sub(*at)/time.Second))
			report.Projections[name] = &freshness.ProjectionFreshness{ComputedAt: at, AgeSeconds: &age, Stale: age > StaleAfterSeconds}
		}
	}
	if _, ok := report.Projections["activity"]; !ok {
		report.Projections["activity"] = nil
	}
	report.Stale = report.Stale || in.ContactStateStale || in.OmittedStale || in.Unavailable
	for _, s := range in.Sources[min(10, len(in.Sources)):] {
		report.Stale = report.Stale || s.LastSuccessfulSyncAt == nil || in.CollectedAt.Sub(*s.LastSuccessfulSyncAt) > time.Duration(StaleAfterSeconds)*time.Second
	}
	warnings := []string{}
	for _, s := range report.Sources {
		if s.Stale {
			label := "has no successful sync"
			if s.AgeSeconds != nil {
				label = fmt.Sprintf("last synced %dd %dh ago", *s.AgeSeconds/86400, (*s.AgeSeconds%86400)/3600)
			}
			warnings = append(warnings, "source "+s.Identifier+" "+label)
		}
	}
	report.Warnings = append(report.Warnings, warnings...)
	more := len(report.Warnings) > 5
	report.Warnings = report.Warnings[:min(5, len(report.Warnings))]
	wrapped := make([]SourceFreshness, 0, len(report.Sources))
	for _, v := range report.Sources {
		for _, raw := range in.Sources[:min(10, len(in.Sources))] {
			if v.SourceID != nil && *v.SourceID == raw.SourceID {
				reason, _ := clean(raw.SyncUnavailableReason, 120)
				if syncCommand(raw) == "" && raw.CanSync {
					reason = "use list_source_status"
				}
				_, more := clean(raw.Identifier, 120)
				wrapped = append(wrapped, SourceFreshness{SourceFreshness: v, CanSync: raw.CanSync, SyncUnavailableReason: reason, IdentifierMore: more})
				break
			}
		}
	}
	return FreshnessBlock{Report: report, Sources: wrapped, CollectedAt: in.CollectedAt.UTC(), SourcesOmitted: in.SourcesOmitted + max(0, len(in.Sources)-10), WarningsMore: more}
}
func (b FreshnessBlock) SourceStale(id int64) bool {
	if id <= 0 {
		return false
	}
	for _, s := range b.Sources {
		if s.SourceID != nil && *s.SourceID == id {
			return s.Stale
		}
	}
	return true
}
func syncCommand(s SourceInput) string {
	if !s.CanSync {
		return ""
	}
	switch s.SourceType {
	case "gmail", "imap", "msmail", "outlook", "exchange", "email":
		return "msgvault sync-incremental " + shellArgument(s.Identifier)
	case "google_calendar", "gcal":
		// Archive keys include the calendar; the sync command takes its account.
		account, _, _ := strings.Cut(s.Identifier, "/")
		return "msgvault sync-calendar " + shellArgument(account)
	default:
		return ""
	}
}
func shellArgument(s string) string {
	s, _ = clean(s, 120)
	if s != "" && !strings.ContainsAny(s, " \t\r\n'\"`$;&|<>()\\*?![]{}") && !strings.HasPrefix(s, "-") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
