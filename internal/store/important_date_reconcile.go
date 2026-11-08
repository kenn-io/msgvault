package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ImportantDateIdentityDecision maps one historical subject and meaning to
// every live subject that may represent it. Ambiguity must guard every possible
// destination; an owned subject keeps its UID and needs an explicit owner.
type ImportantDateIdentityDecision struct {
	OriginalSubject  string   `json:"original_subject"`
	Meaning          string   `json:"meaning"`
	ResolvedSubjects []string `json:"resolved_subjects"`
	EffectiveOwner   string   `json:"effective_owner"`
}

type ImportantDateReconcileRequest struct {
	PersonID           int64                           `json:"person_id"`
	IdentityRevision   int64                           `json:"identity_revision"`
	LineageDigest      string                          `json:"lineage_digest"`
	IdempotencyKey     string                          `json:"idempotency_key"`
	Actor              string                          `json:"actor"`
	InspectedSnapshots bool                            `json:"inspected_snapshots"`
	Decisions          []ImportantDateIdentityDecision `json:"decisions"`
}

// ReconcileImportantDateIdentityContext records a reviewed mapping and guards
// every retained occurrence without changing history or enabling publication.
// Publication remains quarantined until routing and guard checks consume these
// decisions under the same identity fence.
func (s *Store) ReconcileImportantDateIdentityContext(ctx context.Context, request ImportantDateReconcileRequest) (string, error) {
	if request.PersonID <= 0 || request.LineageDigest == "" || strings.TrimSpace(request.IdempotencyKey) == "" || strings.TrimSpace(request.Actor) == "" {
		return "", errors.New("invalid important-date reconciliation request")
	}
	encoded, err := json.Marshal(request, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode important-date reconciliation: %w", err)
	}
	hash := sha256.Sum256(encoded)
	requestHash := hex.EncodeToString(hash[:])
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("create reconciliation identity: %w", err)
	}
	result := ""
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var existingUID, existingHash string
		err := tx.QueryRowContext(ctx, `SELECT uid, request_hash FROM important_date_reconciliations WHERE idempotency_key = ?`, request.IdempotencyKey).Scan(&existingUID, &existingHash)
		if err == nil {
			if existingHash != requestHash {
				return errors.New("important-date reconciliation key already used for another request")
			}
			result = existingUID
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read important-date reconciliation: %w", err)
		}
		fenced := withWritableSnapshot(ctx, s.db.DB, tx)
		lineage, err := s.ImportantDateLineageContext(fenced, request.PersonID)
		if err != nil {
			return err
		}
		if lineage.IdentityRevision != request.IdentityRevision || lineage.Digest != request.LineageDigest {
			return errors.New("important-date lineage changed; inspect it again")
		}
		if len(lineage.MissingPersonIDs) > 0 && !request.InspectedSnapshots {
			return errors.New("inspect merge snapshots before reconciling deleted identities")
		}
		if len(lineage.MissingSplitIDs) > 0 {
			return errors.New("split outcome is unavailable; identity remains quarantined")
		}
		for _, split := range lineage.SplitResults {
			if len(split.AmbiguousRows) > 0 || len(split.UnrestoredRows) > 0 {
				return errors.New("split has unresolved evidence; identity remains quarantined")
			}
		}
		for _, mergeID := range lineage.MergeIDs {
			if _, err := s.GetPersonMergeSnapshotContext(fenced, mergeID); err != nil {
				return fmt.Errorf("verify merge snapshot: %w", err)
			}
		}
		for _, delivery := range lineage.Deliveries {
			switch delivery.State {
			case ImportantDateClaimed, ImportantDateIssueInFlight, ImportantDateIssueCreated, ImportantDateNotifyInFlight, ImportantDateUnknown:
				return errors.New("resolve uncertain or pending deliveries before reconciliation")
			}
		}
		return s.recordImportantDateReconciliationTx(fenced, tx, request, lineage, requestHash, id.String(), &result)
	})
	return result, err
}

type importantDateDecisionKey struct {
	subject string
	meaning string
}

func (s *Store) recordImportantDateReconciliationTx(ctx context.Context, tx *loggedTx, request ImportantDateReconcileRequest, lineage ImportantDateLineage, requestHash, uid string, result *string) error {
	if len(request.IdempotencyKey) > 128 || len(request.Actor) > 128 {
		return errors.New("important-date reconciliation key or actor is too long")
	}
	live := map[string]bool{}
	rootUID := ""
	for _, person := range lineage.People {
		live["person/"+person.UID] = true
		if person.ID == request.PersonID {
			rootUID = person.UID
		}
	}
	if rootUID == "" {
		return ErrPersonNotFound
	}
	expected := map[importantDateDecisionKey]bool{}
	for _, personUID := range lineage.PersonUIDs {
		for _, meaning := range []string{"birthday", "anniversary"} {
			expected[importantDateDecisionKey{subject: "person/" + personUID, meaning: meaning}] = true
		}
	}
	for _, subject := range lineage.OwnedSubjects {
		expected[importantDateDecisionKey{subject: "owned/" + subject.UID, meaning: "birthday"}] = true
	}
	decisions := make(map[importantDateDecisionKey]ImportantDateIdentityDecision, len(request.Decisions))
	for _, decision := range request.Decisions {
		key := importantDateDecisionKey{subject: decision.OriginalSubject, meaning: decision.Meaning}
		if !expected[key] || len(decision.ResolvedSubjects) == 0 {
			return errors.New("incomplete or unknown important-date identity decision")
		}
		if _, exists := decisions[key]; exists {
			return errors.New("duplicate important-date identity decision")
		}
		targets := map[string]bool{}
		for _, target := range decision.ResolvedSubjects {
			if targets[target] {
				return errors.New("duplicate important-date identity target")
			}
			targets[target] = true
			if strings.HasPrefix(key.subject, "owned/") {
				if target != key.subject || !live[decision.EffectiveOwner] {
					return errors.New("owned subject needs itself and an explicit live owner")
				}
			} else if !live[target] || decision.EffectiveOwner != "" {
				return errors.New("person decision requires live person targets")
			}
		}
		if live[key.subject] && !targets[key.subject] {
			return errors.New("a live person's own identity must remain a possible target")
		}
		decisions[key] = decision
	}
	if len(decisions) != len(expected) {
		return errors.New("every lineage subject and meaning needs an explicit decision")
	}
	// A new decision supersedes every earlier decision that touches this
	// lineage, without discarding its bindings, guards, or audit record.
	for key := range expected {
		if _, err := tx.ExecContext(ctx, `UPDATE important_date_reconciliations SET active = 0 WHERE uid IN (SELECT reconciliation_uid FROM important_date_identity_bindings WHERE original_subject = ?)`, key.subject); err != nil {
			return fmt.Errorf("supersede prior important-date decisions: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO important_date_reconciliations (uid, idempotency_key, request_hash, lineage_digest, topology_digest, identity_revision, root_person_uid, actor) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, uid, request.IdempotencyKey, requestHash, request.LineageDigest, lineage.TopologyDigest, request.IdentityRevision, rootUID, request.Actor); err != nil {
		return fmt.Errorf("record important-date reconciliation: %w", err)
	}
	for _, decision := range request.Decisions {
		for _, target := range decision.ResolvedSubjects {
			if _, err := tx.ExecContext(ctx, `INSERT INTO important_date_identity_bindings (reconciliation_uid, original_subject, meaning, resolved_subject, effective_owner) VALUES (?, ?, ?, ?, ?)`, uid, decision.OriginalSubject, decision.Meaning, target, decision.EffectiveOwner); err != nil {
				return fmt.Errorf("record important-date identity binding: %w", err)
			}
		}
	}
	for _, delivery := range lineage.Deliveries {
		if delivery.State == ImportantDateRetryIssue {
			continue // No external mutation was attempted.
		}
		yearAt := strings.LastIndexByte(delivery.OccurrenceKey, ':')
		if yearAt < 1 {
			return errors.New("invalid retained important-date occurrence")
		}
		meaningAt := strings.LastIndexByte(delivery.OccurrenceKey[:yearAt], ':')
		if meaningAt < 1 {
			return errors.New("invalid retained important-date occurrence")
		}
		year, err := strconv.Atoi(delivery.OccurrenceKey[yearAt+1:])
		if err != nil || year < 1 || year > 9999 {
			return errors.New("invalid retained important-date occurrence year")
		}
		key := importantDateDecisionKey{subject: delivery.OccurrenceKey[:meaningAt], meaning: delivery.OccurrenceKey[meaningAt+1 : yearAt]}
		decision, ok := decisions[key]
		if !ok {
			return errors.New("retained occurrence has no identity decision")
		}
		for _, target := range decision.ResolvedSubjects {
			if _, err := tx.ExecContext(ctx, `INSERT INTO important_date_occurrence_guards (reconciliation_uid, resolved_subject, meaning, occurrence_year, original_occurrence_key) VALUES (?, ?, ?, ?, ?)`, uid, target, key.meaning, year, delivery.OccurrenceKey); err != nil {
				return fmt.Errorf("retain important-date occurrence guard: %w", err)
			}
		}
	}
	for key := range expected {
		if _, err := tx.ExecContext(ctx, `UPDATE important_date_consents SET granted = 0 WHERE subject_uid = ?`, key.subject); err != nil {
			return fmt.Errorf("revoke reconciled important-date consent: %w", err)
		}
	}
	*result = uid
	return nil
}
