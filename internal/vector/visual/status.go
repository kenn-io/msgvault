package visual

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.kenn.io/msgvault/internal/store"
)

type ProviderUsage struct {
	Requests       int64   `json:"requests"`
	InputBytes     int64   `json:"input_bytes"`
	BilledUnits    float64 `json:"billed_units"`
	UsageAvailable bool    `json:"usage_available"`
}

type FormatCoverage struct {
	MIMEType  string `json:"mime_type"`
	Eligible  int64  `json:"eligible"`
	Current   int64  `json:"current"`
	Stale     int64  `json:"stale"`
	Terminal  int64  `json:"terminal"`
	Retryable int64  `json:"retryable"`
	Bytes     int64  `json:"bytes"`
}

type DuplicateCostRisk struct {
	AtLeastOnce        bool   `json:"at_least_once"`
	ProviderIdempotent bool   `json:"provider_idempotent"`
	Detail             string `json:"detail"`
}

type Status struct {
	Generation       store.VisualGeneration `json:"generation"`
	Eligible         int64                  `json:"eligible"`
	Current          int64                  `json:"current"`
	Stale            int64                  `json:"stale"`
	Tombstoned       int64                  `json:"tombstoned"`
	Retryable        int64                  `json:"retryable"`
	Terminal         int64                  `json:"terminal"`
	UnknownRole      int64                  `json:"unknown_role"`
	Unavailable      int64                  `json:"unavailable"`
	ActiveLeases     int64                  `json:"active_leases"`
	JournalHighWater int64                  `json:"journal_high_water"`
	// ReconciliationComplete reports whether the consumer's full baseline
	// scan has finished. A capability-manifest change reopens it while the
	// generation stays active and rebaselines the journal cursor, so zero
	// lag plus converged existing publications does NOT mean the re-scan
	// visited every attachment.
	ReconciliationComplete bool              `json:"reconciliation_complete"`
	JournalCursor          int64             `json:"journal_cursor"`
	JournalLag             int64             `json:"journal_lag"`
	Converged              int64             `json:"converged"`
	ConvergenceTotal       int64             `json:"convergence_total"`
	ConvergenceRatio       float64           `json:"convergence_ratio"`
	Formats                []FormatCoverage  `json:"formats"`
	Usage                  ProviderUsage     `json:"usage"`
	DuplicateCost          DuplicateCostRisk `json:"duplicate_cost"`
}

// Status reports authoritative archive and publication state. Provider usage
// is supplied by the caller because this lifecycle layer does not invent
// billing data when the provider omits it.
// Status reports lane progress. includeCoverage additionally scans and
// re-inspects every candidate's media to compute eligibility and per-format
// coverage — a full blob read of the archive — so scheduled passes and build
// loops must pass false and only operator-facing status displays pass true.
func (r *Reconciler) Status(
	ctx context.Context,
	usage ProviderUsage,
	providerIdempotent bool,
	includeCoverage bool,
) (Status, error) {
	status := Status{Usage: usage}
	generation, err := r.archive.GetVisualGeneration(ctx, r.config.GenerationID)
	if err != nil {
		return Status{}, err
	}
	status.Generation = generation
	consumer, err := r.archive.GetAttachmentChangeConsumer(ctx, r.config.ConsumerKey)
	if err != nil && !errors.Is(err, store.ErrAttachmentChangeConsumerMissing) {
		return Status{}, err
	}
	// A generation that has never run a build pass (or was just retired) has
	// no registered consumer yet. Report it as fully unreconciled — cursor
	// zero, everything lagging — instead of failing the status call. The
	// zero-value consumer already encodes that state, and registration stays
	// with the build path so read-only status calls never write.
	status.JournalCursor = consumer.LastSequence
	status.ReconciliationComplete = consumer.ReconciliationComplete
	status.JournalHighWater, err = r.archive.AttachmentChangeHighWater(ctx)
	if err != nil {
		return Status{}, err
	}
	if status.JournalHighWater > status.JournalCursor {
		status.JournalLag = status.JournalHighWater - status.JournalCursor
	}
	now := r.config.Now()
	status.ActiveLeases, err = r.archive.CountActiveVisualClaims(ctx, r.config.GenerationID, now)
	if err != nil {
		return Status{}, err
	}

	// Status scans are read-only walks, not paid-work batches: pinning them
	// to the reconciler's two-owner provider page size made recomputing
	// status after every bounded build pass quadratic in round trips.
	statusPageSize := max(r.config.PageSize, 500)
	formats := make(map[string]*FormatCoverage)
	ownerFormats := make(map[store.VisualOwner]string)
	for after := int64(0); includeCoverage; {
		page, pageErr := r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
			AfterMessageID: after, LimitMessages: statusPageSize,
			MessageTypes: r.config.MessageTypes, SourceIDs: r.config.SourceIDs,
		})
		if pageErr != nil {
			return Status{}, pageErr
		}
		status.UnknownRole += page.Counts.UnknownRoleOccurrences
		status.Unavailable += page.Counts.UnavailableOccurrences
		for _, candidate := range page.Candidates {
			eligibility, inspectErr := InspectMedia(ctx, r.opener, Occurrence{
				MessageID: candidate.Owner.MessageID, BlobHash: candidate.Owner.BlobHash,
				DeclaredMIME: candidate.DeclaredMIME,
				Role:         candidate.Role, RoleSource: candidate.RoleSource,
			}, r.config.MediaPolicy)
			if inspectErr != nil {
				return Status{}, fmt.Errorf("inspect visual status candidate for message %d: %w",
					candidate.Owner.MessageID, inspectErr)
			}
			if !eligibility.Eligible {
				if eligibility.Reason == ReasonContentUnavailable {
					status.Unavailable++
				}
				continue
			}
			status.Eligible++
			mimeType := eligibility.Media.MIMEType
			coverage := formats[mimeType]
			if coverage == nil {
				coverage = &FormatCoverage{MIMEType: mimeType}
				formats[mimeType] = coverage
			}
			coverage.Eligible++
			coverage.Bytes += int64(len(eligibility.Media.Bytes))
			ownerFormats[candidate.Owner] = mimeType
		}
		if !page.HasMore {
			break
		}
		after = page.NextAfterMessageID
	}

	if !includeCoverage && status.ReconciliationComplete {
		// One aggregate query instead of walking every publication row, and
		// only once the baseline is complete: the build loop requests
		// status after each bounded two-owner pass but cannot exit while
		// the scan is open, so aggregating during those O(N) passes made
		// initial indexing quadratic in publication reads for no decision
		// value. Counters read zero until the scan completes. The bucket
		// logic mirrors the coverage walk below.
		tallies, err := r.archive.CountVisualPublications(ctx, r.config.GenerationID)
		if err != nil {
			return Status{}, err
		}
		for _, tally := range tallies {
			switch tally.State {
			case store.VisualPublicationCurrent:
				status.Current += tally.Count
				status.Converged += tally.Count
			case store.VisualPublicationStale:
				if tally.OutcomeKind == "" {
					status.Stale += tally.Count
				}
			case store.VisualPublicationTombstoned:
				status.Tombstoned += tally.Count
			}
			switch tally.OutcomeKind {
			case OutcomeTerminal:
				status.Terminal += tally.Count
				if tally.State != store.VisualPublicationCurrent {
					status.Converged += tally.Count
				}
			case OutcomeRetryable:
				if tally.State != store.VisualPublicationTombstoned {
					status.Retryable += tally.Count
				}
			}
		}
	}
	for after := int64(0); includeCoverage; {
		page, pageErr := r.archive.ListVisualPublications(ctx, r.config.GenerationID, store.VisualPublicationFilter{
			AfterMessageID: after, LimitMessages: statusPageSize,
		})
		if pageErr != nil {
			return Status{}, pageErr
		}
		for _, publication := range page.Publications {
			coverage := formats[ownerFormats[publication.Owner]]
			switch publication.State {
			case store.VisualPublicationCurrent:
				status.Current++
				if coverage != nil {
					coverage.Current++
				}
				if coverage != nil || !includeCoverage {
					status.Converged++
				}
			case store.VisualPublicationStale:
				// A stale row with a durable outcome is terminal or retryable,
				// not unresolved work; counting it here would hold activation
				// open forever.
				if publication.OutcomeKind == "" {
					status.Stale++
					if coverage != nil {
						coverage.Stale++
					}
				}
			case store.VisualPublicationTombstoned:
				status.Tombstoned++
			}
			switch publication.OutcomeKind {
			case OutcomeTerminal:
				status.Terminal++
				if publication.State != store.VisualPublicationCurrent &&
					(coverage != nil || !includeCoverage) {
					status.Converged++
				}
				if coverage != nil {
					coverage.Terminal++
				}
			case OutcomeRetryable:
				// A tombstoned owner has no live occurrence left to retry;
				// counting it would hold activation open forever.
				if publication.State != store.VisualPublicationTombstoned {
					status.Retryable++
					if coverage != nil {
						coverage.Retryable++
					}
				}
			}
		}
		if !page.HasMore {
			break
		}
		after = page.NextAfterMessageID
	}

	status.ConvergenceTotal = status.Eligible
	if !includeCoverage {
		// Without the coverage scan, convergence is measured over evaluated
		// owners: reconciliation completeness plus a drained journal already
		// guarantee every candidate was visited.
		status.ConvergenceTotal = status.Converged + status.Stale + status.Retryable
	}
	switch {
	case status.ConvergenceTotal > 0:
		status.ConvergenceRatio = float64(status.Converged) / float64(status.ConvergenceTotal)
	case !status.ReconciliationComplete:
		// Counters were skipped while the baseline scan is open; an empty
		// total means "unknown", not "done".
		status.ConvergenceRatio = 0
	default:
		status.ConvergenceRatio = 1
	}
	status.Formats = make([]FormatCoverage, 0, len(formats))
	for _, coverage := range formats {
		status.Formats = append(status.Formats, *coverage)
	}
	sort.Slice(status.Formats, func(i, j int) bool {
		return status.Formats[i].MIMEType < status.Formats[j].MIMEType
	})
	status.DuplicateCost = duplicateCostRisk(providerIdempotent)
	return status, nil
}

func duplicateCostRisk(providerIdempotent bool) DuplicateCostRisk {
	if providerIdempotent {
		return DuplicateCostRisk{
			ProviderIdempotent: true,
			Detail:             "provider idempotency suppresses duplicate paid requests",
		}
	}
	return DuplicateCostRisk{
		AtLeastOnce: true,
		Detail: "provider calls are at least once; a lost lease or provider-success/local-crash " +
			"boundary can repeat a paid request",
	}
}
