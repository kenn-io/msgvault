package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personenrichment"
)

type PersonEnrichmentSuppressionReason string

const (
	PersonEnrichmentSuppressionDeletion           PersonEnrichmentSuppressionReason = "deletion"
	PersonEnrichmentSuppressionOptOut             PersonEnrichmentSuppressionReason = "opt_out"
	PersonEnrichmentSuppressionDataSubjectRequest PersonEnrichmentSuppressionReason = "data_subject_request"
)

// PersonEnrichmentSuppressionInput contains only a provider-scoped keyed
// digest and audit metadata. Raw and normalized identifiers are never stored.
type PersonEnrichmentSuppressionInput struct {
	ProviderNamespace    string
	IdentifierClass      personenrichment.SuppressionIdentifierClass
	NormalizationVersion string
	KeyID                string
	Digest               []byte
	Reason               PersonEnrichmentSuppressionReason
	Actor                string
}

// PersonEnrichmentSuppressionLookup is the narrow egress lookup contract.
type PersonEnrichmentSuppressionLookup = personenrichment.SuppressionLookup

// PersonEnrichmentSuppressionFilter bounds and scopes audit listings.
type PersonEnrichmentSuppressionFilter struct {
	ProviderNamespace    string
	IdentifierClass      personenrichment.SuppressionIdentifierClass
	NormalizationVersion string
	KeyID                string
	Limit                int
}

// PersonEnrichmentSuppression is a redacted audit view. It never returns the
// full digest or any recoverable identifier.
type PersonEnrichmentSuppression struct {
	ProviderNamespace    string                                      `json:"provider_namespace"`
	IdentifierClass      personenrichment.SuppressionIdentifierClass `json:"identifier_class"`
	NormalizationVersion string                                      `json:"normalization_version"`
	KeyID                string                                      `json:"key_id"`
	DigestPrefix         string                                      `json:"digest_prefix"`
	Reason               PersonEnrichmentSuppressionReason           `json:"reason"`
	Actor                string                                      `json:"actor"`
	CreatedAt            time.Time                                   `json:"created_at"`
}

const personEnrichmentDigestPrefixBytes = 6

// InsertPersonEnrichmentSuppressionsContext atomically inserts exact tuples.
// Repeating a tuple is idempotent only when its non-key audit metadata agrees.
func (s *Store) InsertPersonEnrichmentSuppressionsContext(
	ctx context.Context,
	inputs []PersonEnrichmentSuppressionInput,
) error {
	validated := make([]PersonEnrichmentSuppressionInput, len(inputs))
	for i := range inputs {
		input, err := validatePersonEnrichmentSuppressionInput(inputs[i])
		if err != nil {
			return fmt.Errorf("validate person enrichment suppression %d: %w", i, err)
		}
		validated[i] = input
	}
	sort.Slice(validated, func(i, j int) bool {
		return personEnrichmentSuppressionLockKey(
			validated[i].ProviderNamespace, validated[i].IdentifierClass,
			validated[i].NormalizationVersion, validated[i].KeyID, validated[i].Digest,
		) < personEnrichmentSuppressionLockKey(
			validated[j].ProviderNamespace, validated[j].IdentifierClass,
			validated[j].NormalizationVersion, validated[j].KeyID, validated[j].Digest,
		)
	})
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		personIDs, err := s.lockPersonEnrichmentSuppressionAffectedPeopleTx(
			ctx, tx, validated)
		if err != nil {
			return err
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("suppression_affected_people_snapshotted")
		}
		if err := s.lockPersonEnrichmentSuppressionKeyStateTx(ctx, tx); err != nil {
			return err
		}
		for i := range validated {
			input := validated[i]
			if err := s.lockProfileIdentityKeyTxContext(
				ctx, tx, "person-enrichment-suppression",
				personEnrichmentSuppressionLockKey(
					input.ProviderNamespace, input.IdentifierClass,
					input.NormalizationVersion, input.KeyID, input.Digest,
				)); err != nil {
				return fmt.Errorf("lock person enrichment suppression %d: %w", i, err)
			}
			result, err := tx.ExecContext(ctx, `
				INSERT INTO person_enrichment_suppressions
					(provider_namespace, identifier_class, normalization_version,
					 key_id, digest, reason, actor)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (provider_namespace, identifier_class, normalization_version,
				             key_id, digest) DO NOTHING`,
				input.ProviderNamespace, input.IdentifierClass, input.NormalizationVersion,
				input.KeyID, input.Digest, input.Reason, input.Actor,
			)
			if err != nil {
				return fmt.Errorf("insert person enrichment suppression %d: %w", i, err)
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("read person enrichment suppression %d insert result: %w", i, err)
			}
			if rows == 1 {
				continue
			}
			var reason PersonEnrichmentSuppressionReason
			var actor string
			err = tx.QueryRowContext(ctx, `
				SELECT reason, actor FROM person_enrichment_suppressions
				WHERE provider_namespace = ? AND identifier_class = ?
				  AND normalization_version = ? AND key_id = ? AND digest = ?`,
				input.ProviderNamespace, input.IdentifierClass, input.NormalizationVersion,
				input.KeyID, input.Digest,
			).Scan(&reason, &actor)
			if err != nil {
				return fmt.Errorf("verify person enrichment suppression %d: %w", i, err)
			}
			if reason != input.Reason || actor != input.Actor {
				return fmt.Errorf("person enrichment suppression %d already exists with different metadata", i)
			}
		}
		concurrentPersonIDs, err := s.lockCurrentPersonEnrichmentSuppressionAffectedPeopleTx(
			ctx, tx, validated)
		if err != nil {
			return err
		}
		personIDs = sortedUniqueInt64s(append(personIDs, concurrentPersonIDs...)...)
		return s.forceInvalidatePersonEnrichmentPeopleTx(ctx, tx, personIDs)
	})
}

// InsertPersonEnrichmentSuppressionsForConfiguredKeyContext verifies the
// caller's exact configured key against every durable suppression key and
// inserts in the same serialized transaction. This closes the empty-ledger and
// concurrent check/insert windows at service boundaries that own the key.
func (s *Store) InsertPersonEnrichmentSuppressionsForConfiguredKeyContext(
	ctx context.Context,
	configuredKeyID string,
	inputs []PersonEnrichmentSuppressionInput,
) error {
	return s.insertPersonEnrichmentSuppressionsForConfiguredKeyContext(
		ctx, 0, 0, configuredKeyID, inputs)
}

// InsertPersonEnrichmentSuppressionsForPersonRevisionContext inserts a
// person-wide identifier snapshot only if it still describes the locked
// person revision.
func (s *Store) InsertPersonEnrichmentSuppressionsForPersonRevisionContext(
	ctx context.Context,
	personID, expectedRevision int64,
	configuredKeyID string,
	inputs []PersonEnrichmentSuppressionInput,
) error {
	if personID <= 0 || expectedRevision <= 0 {
		return errors.New("person enrichment suppression snapshot is invalid")
	}
	return s.insertPersonEnrichmentSuppressionsForConfiguredKeyContext(
		ctx, personID, expectedRevision, configuredKeyID, inputs)
}

func (s *Store) insertPersonEnrichmentSuppressionsForConfiguredKeyContext(
	ctx context.Context,
	personID, expectedRevision int64,
	configuredKeyID string,
	inputs []PersonEnrichmentSuppressionInput,
) error {
	if !validLowerSHA256(configuredKeyID) {
		return personenrichment.ErrSuppressionKeyMismatch
	}
	validated := make([]PersonEnrichmentSuppressionInput, len(inputs))
	for i := range inputs {
		input, err := validatePersonEnrichmentSuppressionInput(inputs[i])
		if err != nil {
			return fmt.Errorf("validate person enrichment suppression %d: %w", i, err)
		}
		if input.KeyID != configuredKeyID {
			return personenrichment.ErrSuppressionKeyMismatch
		}
		validated[i] = input
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		if personID > 0 && s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("person_suppression_before_person_lock")
		}
		personIDs, err := s.lockCurrentPersonEnrichmentSuppressionAffectedPeopleTx(
			ctx, tx, validated, personID)
		if err != nil {
			return err
		}
		if s.personEnrichmentTxBarrier != nil {
			s.personEnrichmentTxBarrier("suppression_affected_people_snapshotted")
		}
		if personID > 0 {
			var revision int64
			err := tx.QueryRowContext(ctx,
				`SELECT revision FROM persons WHERE id = ?`, personID).Scan(&revision)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPersonNotFound
			}
			if err != nil {
				return fmt.Errorf("verify person enrichment suppression snapshot: %w", err)
			}
			if revision != expectedRevision {
				return ErrPersonRevisionConflict
			}
		}
		if err := s.validatePersonEnrichmentSuppressionKeyStateTx(
			ctx, tx, configuredKeyID); err != nil {
			return err
		}
		if err := s.insertPersonEnrichmentSuppressionsTx(ctx, tx, validated); err != nil {
			return err
		}
		concurrentPersonIDs, err := s.lockCurrentPersonEnrichmentSuppressionAffectedPeopleTx(
			ctx, tx, validated)
		if err != nil {
			return err
		}
		personIDs = sortedUniqueInt64s(append(personIDs, concurrentPersonIDs...)...)
		return s.forceInvalidatePersonEnrichmentPeopleTx(ctx, tx, personIDs)
	})
}

func (s *Store) lockPersonEnrichmentSuppressionAffectedPeopleTx(
	ctx context.Context,
	tx *loggedTx,
	inputs []PersonEnrichmentSuppressionInput,
) ([]int64, error) {
	if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
		return nil, err
	}
	return s.lockCurrentPersonEnrichmentSuppressionAffectedPeopleTx(ctx, tx, inputs)
}

func (s *Store) lockCurrentPersonEnrichmentSuppressionAffectedPeopleTx(
	ctx context.Context,
	tx *loggedTx,
	inputs []PersonEnrichmentSuppressionInput,
	includePersonIDs ...int64,
) ([]int64, error) {
	affected := make(map[int64]struct{}, len(includePersonIDs))
	for _, personID := range includePersonIDs {
		if personID > 0 {
			affected[personID] = struct{}{}
		}
	}
	for i := range inputs {
		input := inputs[i]
		rows, err := tx.QueryContext(ctx, `SELECT a.person_id
			FROM person_enrichment_attempts a
			JOIN person_enrichment_attempt_identifiers identifier ON identifier.attempt_id = a.id
			WHERE identifier.provider_namespace = ? AND identifier.identifier_class = ?
			  AND identifier.normalization_version = ? AND identifier.key_id = ?
			  AND identifier.digest = ?
			  AND a.state IN ('queued','starting','pending','retry_wait','uncertain_start')
			GROUP BY a.person_id ORDER BY a.person_id`, input.ProviderNamespace,
			input.IdentifierClass, input.NormalizationVersion, input.KeyID, input.Digest)
		if err != nil {
			return nil, fmt.Errorf("list people affected by person enrichment suppression %d: %w", i, err)
		}
		for rows.Next() {
			var personID int64
			if err := rows.Scan(&personID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan person affected by person enrichment suppression %d: %w", i, err)
			}
			affected[personID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate people affected by person enrichment suppression %d: %w", i, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close people affected by person enrichment suppression %d: %w", i, err)
		}
	}
	personIDs := make([]int64, 0, len(affected))
	for personID := range affected {
		personIDs = append(personIDs, personID)
	}
	slices.Sort(personIDs)
	for _, personID := range personIDs {
		if _, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, personID); err != nil {
			return nil, err
		}
	}
	return personIDs, nil
}

func (s *Store) forceInvalidatePersonEnrichmentPeopleTx(
	ctx context.Context, tx *loggedTx, personIDs []int64,
) error {
	for _, personID := range personIDs {
		if err := s.forceInvalidatePersonEnrichmentTx(ctx, tx, personID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) lockPersonEnrichmentSuppressionKeyStateTx(
	ctx context.Context, tx *loggedTx,
) error {
	return s.lockProfileIdentityKeyTxContext(
		ctx, tx, "person-enrichment-suppression-key-state", "global")
}

func (s *Store) validatePersonEnrichmentSuppressionKeyStateTx(
	ctx context.Context, tx *loggedTx, configuredKeyID string,
) error {
	if !validLowerSHA256(configuredKeyID) {
		return personenrichment.ErrSuppressionKeyMismatch
	}
	if err := s.lockPersonEnrichmentSuppressionKeyStateTx(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT key_id FROM (
			SELECT key_id FROM person_enrichment_suppressions
			UNION
			SELECT key_id FROM person_enrichment_attempt_identifiers
		) durable_keys ORDER BY key_id`)
	if err != nil {
		return fmt.Errorf("load durable person enrichment suppression key state: %w", err)
	}
	for rows.Next() {
		var durableKeyID string
		if err := rows.Scan(&durableKeyID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read durable person enrichment suppression key state: %w", err)
		}
		if durableKeyID != configuredKeyID {
			_ = rows.Close()
			return personenrichment.ErrSuppressionKeyMismatch
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate durable person enrichment suppression key state: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close durable person enrichment suppression key state: %w", err)
	}
	return nil
}

// validatePersonEnrichmentAttemptIdentifierKeyStateTx serializes every
// attempt-identifier writer with suppression and deletion key-state changes.
// All identifiers written by one operation must come from the same exact key,
// and that key must match every durable identifier and suppression row.
func (s *Store) validatePersonEnrichmentAttemptIdentifierKeyStateTx(
	ctx context.Context, tx *loggedTx, digests []personenrichment.SuppressionDigest,
) error {
	if len(digests) == 0 {
		return nil
	}
	keyID := digests[0].KeyID
	for i := 1; i < len(digests); i++ {
		if digests[i].KeyID != keyID {
			return personenrichment.ErrSuppressionKeyMismatch
		}
	}
	return s.validatePersonEnrichmentSuppressionKeyStateTx(ctx, tx, keyID)
}

func (s *Store) loadPersonEnrichmentAttemptIdentifiersTx(
	ctx context.Context, tx *loggedTx, attemptID int64,
) ([]personenrichment.SuppressionDigest, error) {
	rows, err := tx.QueryContext(ctx, `SELECT provider_namespace, identifier_class,
		normalization_version, key_id, digest
		FROM person_enrichment_attempt_identifiers
		WHERE attempt_id = ?
		ORDER BY provider_namespace, identifier_class, normalization_version, key_id, digest`,
		attemptID)
	if err != nil {
		return nil, fmt.Errorf("load checked person enrichment identifiers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	digests := make([]personenrichment.SuppressionDigest, 0)
	for rows.Next() {
		var digest personenrichment.SuppressionDigest
		if err := rows.Scan(&digest.ProviderNamespace, &digest.IdentifierClass,
			&digest.NormalizationVersion, &digest.KeyID, &digest.Digest); err != nil {
			return nil, fmt.Errorf("scan checked person enrichment identifier: %w", err)
		}
		digests = append(digests, digest)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate checked person enrichment identifiers: %w", err)
	}
	return digests, nil
}

func (s *Store) recheckPersonEnrichmentSuppressionsTx(
	ctx context.Context, tx *loggedTx, input []personenrichment.SuppressionDigest,
) error {
	if len(input) == 0 {
		return nil
	}
	digests := append([]personenrichment.SuppressionDigest(nil), input...)
	for i := range digests {
		if err := validatePersonEnrichmentSuppressionLookup(digests[i]); err != nil {
			return fmt.Errorf("validate checked person enrichment identifier %d: %w", i, err)
		}
	}
	if err := s.validatePersonEnrichmentAttemptIdentifierKeyStateTx(ctx, tx, digests); err != nil {
		return err
	}
	sort.Slice(digests, func(i, j int) bool {
		return personEnrichmentSuppressionLockKey(
			digests[i].ProviderNamespace, digests[i].IdentifierClass,
			digests[i].NormalizationVersion, digests[i].KeyID, digests[i].Digest,
		) < personEnrichmentSuppressionLockKey(
			digests[j].ProviderNamespace, digests[j].IdentifierClass,
			digests[j].NormalizationVersion, digests[j].KeyID, digests[j].Digest,
		)
	})
	previousKey := ""
	for _, digest := range digests {
		lockKey := personEnrichmentSuppressionLockKey(
			digest.ProviderNamespace, digest.IdentifierClass,
			digest.NormalizationVersion, digest.KeyID, digest.Digest)
		if lockKey == previousKey {
			continue
		}
		previousKey = lockKey
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "person-enrichment-suppression", lockKey); err != nil {
			return err
		}
		var suppressed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM person_enrichment_suppressions
			WHERE provider_namespace = ? AND identifier_class = ?
			  AND normalization_version = ? AND key_id = ? AND digest = ?)`,
			digest.ProviderNamespace, digest.IdentifierClass, digest.NormalizationVersion,
			digest.KeyID, digest.Digest).Scan(&suppressed); err != nil {
			return fmt.Errorf("recheck checked person enrichment suppression: %w", err)
		}
		if suppressed {
			return personenrichment.ErrSuppressed
		}
	}
	return nil
}

// insertPersonEnrichmentSuppressionsTx inserts already validated digest-only
// suppressions in deterministic lock order. It intentionally treats an exact
// existing tuple as sufficient: an earlier opt-out or DSR record must not make
// a later person deletion fail or rewrite the original audit metadata.
func (s *Store) insertPersonEnrichmentSuppressionsTx(
	ctx context.Context,
	tx *loggedTx,
	inputs []PersonEnrichmentSuppressionInput,
) error {
	sort.Slice(inputs, func(i, j int) bool {
		return personEnrichmentSuppressionLockKey(
			inputs[i].ProviderNamespace, inputs[i].IdentifierClass,
			inputs[i].NormalizationVersion, inputs[i].KeyID, inputs[i].Digest,
		) < personEnrichmentSuppressionLockKey(
			inputs[j].ProviderNamespace, inputs[j].IdentifierClass,
			inputs[j].NormalizationVersion, inputs[j].KeyID, inputs[j].Digest,
		)
	})
	for i := range inputs {
		input := inputs[i]
		if err := s.lockProfileIdentityKeyTxContext(
			ctx, tx, "person-enrichment-suppression",
			personEnrichmentSuppressionLockKey(
				input.ProviderNamespace, input.IdentifierClass,
				input.NormalizationVersion, input.KeyID, input.Digest,
			)); err != nil {
			return fmt.Errorf("lock person enrichment suppression %d: %w", i, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO person_enrichment_suppressions
				(provider_namespace, identifier_class, normalization_version,
				 key_id, digest, reason, actor)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (provider_namespace, identifier_class, normalization_version,
			             key_id, digest) DO NOTHING`,
			input.ProviderNamespace, input.IdentifierClass, input.NormalizationVersion,
			input.KeyID, input.Digest, input.Reason, input.Actor,
		); err != nil {
			return fmt.Errorf("insert person enrichment suppression %d: %w", i, err)
		}
	}
	return nil
}

// HasPersonEnrichmentSuppressionContext checks one exact provider-scoped
// digest tuple.
func (s *Store) HasPersonEnrichmentSuppressionContext(
	ctx context.Context,
	lookup PersonEnrichmentSuppressionLookup,
) (bool, error) {
	if err := validatePersonEnrichmentSuppressionLookup(lookup); err != nil {
		return false, err
	}
	var found bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM person_enrichment_suppressions
			WHERE provider_namespace = ? AND identifier_class = ?
			  AND normalization_version = ? AND key_id = ? AND digest = ?
		)`, lookup.ProviderNamespace, lookup.IdentifierClass,
		lookup.NormalizationVersion, lookup.KeyID, lookup.Digest,
	).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check person enrichment suppression: %w", err)
	}
	return found, nil
}

// ListPersonEnrichmentSuppressionsContext returns a bounded, redacted audit
// listing. A limit is mandatory so no caller can accidentally dump all rows.
func (s *Store) ListPersonEnrichmentSuppressionsContext(
	ctx context.Context,
	filter PersonEnrichmentSuppressionFilter,
) ([]PersonEnrichmentSuppression, error) {
	if filter.Limit < 1 || filter.Limit > 200 {
		return nil, errors.New("person enrichment suppression list limit must be in [1,200]")
	}
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if filter.ProviderNamespace != "" {
		clauses = append(clauses, "provider_namespace = ?")
		args = append(args, filter.ProviderNamespace)
	}
	if filter.IdentifierClass != "" {
		if !validPersonEnrichmentSuppressionClass(filter.IdentifierClass) {
			return nil, fmt.Errorf("invalid person enrichment suppression class %q", filter.IdentifierClass)
		}
		clauses = append(clauses, "identifier_class = ?")
		args = append(args, filter.IdentifierClass)
	}
	if filter.NormalizationVersion != "" {
		clauses = append(clauses, "normalization_version = ?")
		args = append(args, filter.NormalizationVersion)
	}
	if filter.KeyID != "" {
		if !validLowerSHA256(filter.KeyID) {
			return nil, errors.New("person enrichment suppression filter has invalid key ID")
		}
		clauses = append(clauses, "key_id = ?")
		args = append(args, filter.KeyID)
	}
	query := `
		SELECT provider_namespace, identifier_class, normalization_version,
		       key_id, digest, reason, actor, created_at
		FROM person_enrichment_suppressions`
	if len(clauses) != 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list person enrichment suppressions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]PersonEnrichmentSuppression, 0)
	for rows.Next() {
		var suppression PersonEnrichmentSuppression
		var digest []byte
		var createdAt nullableTimestamp
		if err := rows.Scan(
			&suppression.ProviderNamespace, &suppression.IdentifierClass,
			&suppression.NormalizationVersion, &suppression.KeyID, &digest,
			&suppression.Reason, &suppression.Actor, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("read person enrichment suppression: %w", err)
		}
		if len(digest) != sha256.Size || !createdAt.Valid {
			return nil, errors.New("person enrichment suppression has invalid durable metadata")
		}
		suppression.DigestPrefix = hex.EncodeToString(digest[:personEnrichmentDigestPrefixBytes])
		suppression.CreatedAt = createdAt.Time
		result = append(result, suppression)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list person enrichment suppressions: %w", err)
	}
	return result, nil
}

// ListPersonEnrichmentSuppressionKeyIDsContext returns the complete distinct
// key-ID set needed to fail closed before digest lookup after key rotation.
func (s *Store) ListPersonEnrichmentSuppressionKeyIDsContext(
	ctx context.Context,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id FROM (
			SELECT key_id FROM person_enrichment_suppressions
			UNION
			SELECT key_id FROM person_enrichment_attempt_identifiers
		) durable_keys ORDER BY key_id`)
	if err != nil {
		return nil, fmt.Errorf("list person enrichment suppression key IDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0)
	for rows.Next() {
		var keyID string
		if err := rows.Scan(&keyID); err != nil {
			return nil, fmt.Errorf("read person enrichment suppression key ID: %w", err)
		}
		if !validLowerSHA256(keyID) {
			return nil, errors.New("person enrichment suppression has invalid durable key ID")
		}
		result = append(result, keyID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list person enrichment suppression key IDs: %w", err)
	}
	return result, nil
}

func validatePersonEnrichmentSuppressionInput(
	input PersonEnrichmentSuppressionInput,
) (PersonEnrichmentSuppressionInput, error) {
	lookup := PersonEnrichmentSuppressionLookup{
		ProviderNamespace: input.ProviderNamespace, IdentifierClass: input.IdentifierClass,
		NormalizationVersion: input.NormalizationVersion, KeyID: input.KeyID,
		Digest: input.Digest,
	}
	if err := validatePersonEnrichmentSuppressionLookup(lookup); err != nil {
		return PersonEnrichmentSuppressionInput{}, err
	}
	switch input.Reason {
	case PersonEnrichmentSuppressionDeletion, PersonEnrichmentSuppressionOptOut,
		PersonEnrichmentSuppressionDataSubjectRequest:
	default:
		return PersonEnrichmentSuppressionInput{}, fmt.Errorf(
			"invalid person enrichment suppression reason %q", input.Reason)
	}
	input.Actor = strings.TrimSpace(input.Actor)
	if input.Actor == "" {
		return PersonEnrichmentSuppressionInput{}, errors.New(
			"person enrichment suppression actor is required")
	}
	input.Digest = append([]byte(nil), input.Digest...)
	return input, nil
}

func validatePersonEnrichmentSuppressionLookup(
	lookup PersonEnrichmentSuppressionLookup,
) error {
	kind, _, ok := strings.Cut(lookup.ProviderNamespace, ":")
	if !ok || !validPersonEnrichmentProviderNamespace(lookup.ProviderNamespace, kind) ||
		(kind != personenrichment.ProviderExa && kind != personenrichment.ProviderSixtyfour) {
		return errors.New("person enrichment suppression has invalid provider namespace")
	}
	if !validPersonEnrichmentSuppressionClass(lookup.IdentifierClass) {
		return fmt.Errorf("invalid person enrichment suppression class %q", lookup.IdentifierClass)
	}
	if lookup.NormalizationVersion == "" ||
		lookup.NormalizationVersion != strings.TrimSpace(lookup.NormalizationVersion) {
		return errors.New("person enrichment suppression normalization version is required")
	}
	if !validLowerSHA256(lookup.KeyID) {
		return errors.New("person enrichment suppression requires a lowercase SHA-256 key ID")
	}
	if len(lookup.Digest) != sha256.Size {
		return errors.New("person enrichment suppression digest must be SHA-256 sized")
	}
	return nil
}

func validPersonEnrichmentSuppressionClass(
	class personenrichment.SuppressionIdentifierClass,
) bool {
	switch class {
	case personenrichment.SuppressionEmail, personenrichment.SuppressionPhone,
		personenrichment.SuppressionPublicProfileURL,
		personenrichment.SuppressionProviderPersonID,
		personenrichment.SuppressionNameCompany:
		return true
	default:
		return false
	}
}
