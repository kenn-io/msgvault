package importantdate

import (
	"context"
	"fmt"
	"time"
)

const maxDeliveriesPerRun = 25

type ProducerStore interface {
	CandidateStore
	DeliveryStore
	HasImportantDateConsentContext(context.Context, string, string, string) (bool, error)
	ImportantDateDeliveryExistsContext(context.Context, string) (bool, error)
}

// RunOnce performs a bounded local calendar scan. Every actual write has a
// second scan under the identity and consent fence; no model is involved.
func RunOnce(ctx context.Context, st ProducerStore, kata DeliveryKata, cfg Config, now time.Time) (int, error) {
	if !cfg.Enabled {
		return 0, nil
	}
	candidates, err := Candidates(ctx, st, cfg, now)
	if err != nil {
		return 0, err
	}
	service := DeliveryService{Store: st, Kata: kata}
	attempts := 0
	for _, candidate := range candidates {
		if attempts >= maxDeliveriesPerRun {
			break
		}
		exists, err := st.ImportantDateDeliveryExistsContext(ctx, candidate.OccurrenceKey)
		if err != nil {
			return attempts, err
		}
		if exists {
			continue
		}
		active, err := st.HasImportantDateConsentContext(ctx, candidate.SubjectUID, cfg.Project, cfg.Recipient)
		if err != nil {
			return attempts, err
		}
		if !active {
			continue
		}
		input := DeliveryInput{OccurrenceKey: candidate.OccurrenceKey, Fingerprint: candidate.Fingerprint, SubjectUID: candidate.SubjectUID, OwnerUID: candidate.OwnerUID, Project: cfg.Project, Recipient: cfg.Recipient}
		attempts++
		err = service.Deliver(ctx, input, func(fenced context.Context) (bool, error) {
			current, err := Candidates(fenced, st, cfg, time.Now())
			if err != nil {
				return false, err
			}
			for _, item := range current {
				if item.OccurrenceKey == candidate.OccurrenceKey && item.Fingerprint == candidate.Fingerprint && item.SubjectUID == candidate.SubjectUID && item.OwnerUID == candidate.OwnerUID && item.Tier == candidate.Tier {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			return attempts, fmt.Errorf("important-date delivery failed: %w", err)
		}
	}
	return attempts, nil
}
