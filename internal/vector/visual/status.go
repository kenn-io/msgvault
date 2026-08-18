package visual

import (
	"context"
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
	JournalCursor    int64                  `json:"journal_cursor"`
	JournalLag       int64                  `json:"journal_lag"`
	Converged        int64                  `json:"converged"`
	ConvergenceTotal int64                  `json:"convergence_total"`
	ConvergenceRatio float64                `json:"convergence_ratio"`
	Formats          []FormatCoverage       `json:"formats"`
	Usage            ProviderUsage          `json:"usage"`
	DuplicateCost    DuplicateCostRisk      `json:"duplicate_cost"`
}

// Status reports authoritative archive and publication state. Provider usage
// is supplied by the caller because this lifecycle layer does not invent
// billing data when the provider omits it.
func (r *Reconciler) Status(
	ctx context.Context,
	usage ProviderUsage,
	providerIdempotent bool,
) (Status, error) {
	status := Status{Usage: usage}
	generation, err := r.archive.GetVisualGeneration(ctx, r.config.GenerationID)
	if err != nil {
		return Status{}, err
	}
	status.Generation = generation
	consumer, err := r.archive.GetAttachmentChangeConsumer(ctx, r.config.ConsumerKey)
	if err != nil {
		return Status{}, err
	}
	status.JournalCursor = consumer.LastSequence
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

	formats := make(map[string]*FormatCoverage)
	ownerFormats := make(map[store.VisualOwner]string)
	for after := int64(0); ; {
		page, pageErr := r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
			AfterMessageID: after, LimitMessages: r.config.PageSize,
			MessageTypes: r.config.MessageTypes,
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

	for after := int64(0); ; {
		page, pageErr := r.archive.ListVisualPublications(ctx, r.config.GenerationID, store.VisualPublicationFilter{
			AfterMessageID: after, LimitMessages: r.config.PageSize,
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
					status.Converged++
				}
			case store.VisualPublicationStale:
				status.Stale++
				if coverage != nil {
					coverage.Stale++
				}
			case store.VisualPublicationTombstoned:
				status.Tombstoned++
			}
			switch publication.OutcomeKind {
			case "terminal":
				status.Terminal++
				if publication.State != store.VisualPublicationCurrent && coverage != nil {
					status.Converged++
				}
				if coverage != nil {
					coverage.Terminal++
				}
			case "retryable":
				status.Retryable++
				if coverage != nil {
					coverage.Retryable++
				}
			}
		}
		if !page.HasMore {
			break
		}
		after = page.NextAfterMessageID
	}

	status.ConvergenceTotal = status.Eligible
	if status.ConvergenceTotal > 0 {
		status.ConvergenceRatio = float64(status.Converged) / float64(status.ConvergenceTotal)
	} else {
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
