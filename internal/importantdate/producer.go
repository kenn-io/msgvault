package importantdate

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const maxDeliveriesPerRun = 25

type ProducerStore interface {
	CandidateStore
	DeliveryStore
	HasImportantDateConsentContext(ctx context.Context, subject, project, recipient string) (bool, error)
	ImportantDateDeliveryBlockedContext(ctx context.Context, key, fingerprint string) (bool, error)
}

// RunOnce performs a bounded local calendar scan. Every actual write has a
// second scan under the identity and consent fence; no model is involved.
func RunOnce(ctx context.Context, st ProducerStore, kata DeliveryKata, cfg Config, now time.Time) (int, bool, error) {
	count, next, err := RunBatch(ctx, st, kata, cfg, now, "")
	return count, next != "", err
}

// RunBatch resumes after a prior batch's last attempted key. The cursor is
// local to one scheduled scan; it never authorizes a delivery or bypasses a claim.
func RunBatch(ctx context.Context, st ProducerStore, kata DeliveryKata, cfg Config, now time.Time, after string) (int, string, error) {
	if !cfg.Enabled {
		return 0, "", nil
	}
	if cfg.ConsentScope == "" || cfg.ProjectUID == "" {
		return 0, "", errors.New("important-date destination scope is required")
	}
	candidates, err := Candidates(ctx, st, cfg, now)
	if err != nil {
		return 0, "", err
	}
	service := DeliveryService{Store: st, Kata: kata}
	attempts := 0
	last := after
	var deliveryErrors []error
	for _, candidate := range candidates {
		if candidate.OccurrenceKey <= after {
			continue
		}
		exists, err := st.ImportantDateDeliveryBlockedContext(ctx, candidate.OccurrenceKey, candidate.Fingerprint)
		if err != nil {
			return attempts, "", err
		}
		if exists {
			continue
		}
		active, err := st.HasImportantDateConsentContext(ctx, candidate.SubjectUID, cfg.ConsentScope, cfg.Recipient)
		if err != nil {
			return attempts, "", err
		}
		if !active {
			continue
		}
		if attempts >= maxDeliveriesPerRun {
			return attempts, last, errors.Join(deliveryErrors...)
		}
		last = candidate.OccurrenceKey
		input := DeliveryInput{OccurrenceKey: candidate.OccurrenceKey, Fingerprint: candidate.Fingerprint, SubjectUID: candidate.SubjectUID, OwnerUID: candidate.OwnerUID, Project: cfg.Project, ConsentScope: cfg.ConsentScope, Recipient: cfg.Recipient}
		attempts++
		err = service.Deliver(ctx, input, func(fenced context.Context) (bool, error) {
			projectUID, err := kata.ProjectUID(fenced, cfg.Project)
			if err != nil {
				return false, err
			}
			if projectUID != cfg.ProjectUID {
				return false, nil
			}
			current, err := CandidateForSubject(fenced, st, cfg, time.Now(), candidate.SubjectUID, candidate.OwnerUID)
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
			deliveryErrors = append(deliveryErrors, fmt.Errorf("important-date delivery failed: %w", err))
			if ctx.Err() != nil {
				return attempts, "", errors.Join(deliveryErrors...)
			}
		}
	}
	return attempts, "", errors.Join(deliveryErrors...)
}
