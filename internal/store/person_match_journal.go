package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

var (
	ErrIdentityMatchJudgmentLeaseStale       = errors.New("identity match judgment lease is stale")
	ErrIdentityMatchJudgmentFingerprintStale = errors.New("identity match judgment fingerprint is stale")
	ErrInvalidIdentityMatchJudgment          = errors.New("invalid identity match judgment")
)

// IdentityMatchJudgmentLease authorizes one attempt against one reviewed
// candidate snapshot. Token is an opaque fencing value, never a provider key.
type IdentityMatchJudgmentLease struct {
	CandidateID    int64
	Fingerprint    string
	ScoringVersion string
	Candidate      IdentityMatchCandidate
	Owner          string
	Token          string
	LeaseUntil     time.Time
}

// IdentityMatchJudgmentInput deliberately excludes a packet, identifiers,
// provider response body, and free-form error text.
type IdentityMatchJudgmentInput struct {
	Status          string
	ModelID         string
	QuestionVersion string
	PolicyVersion   string
	Probability     *float64
	Blockers        []string
	Outcome         string
	ErrorClass      string
}

type IdentityMatchJudgment struct {
	ID              int64      `json:"id"`
	CandidateID     int64      `json:"candidate_id"`
	Fingerprint     string     `json:"fingerprint"`
	ModelID         string     `json:"model_id"`
	QuestionVersion string     `json:"question_version"`
	PolicyVersion   string     `json:"policy_version"`
	Probability     *float64   `json:"probability,omitempty"`
	Blockers        []string   `json:"blockers"`
	Outcome         string     `json:"outcome"`
	Status          string     `json:"status"`
	ErrorClass      string     `json:"error_class,omitempty"`
	RetryAfter      *time.Time `json:"retry_after,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

const maxIdentityMatchJudgmentAttempts = 5

var journalLabel = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func validJournalLabel(value string) bool { return journalLabel.MatchString(value) }

func validateIdentityMatchJudgment(input IdentityMatchJudgmentInput) error {
	if !validJournalLabel(input.ModelID) || !validJournalLabel(input.QuestionVersion) ||
		!validJournalLabel(input.PolicyVersion) || !validJournalLabel(input.Outcome) ||
		len(input.Blockers) > 16 {
		return ErrInvalidIdentityMatchJudgment
	}
	for _, blocker := range input.Blockers {
		if !validJournalLabel(blocker) {
			return ErrInvalidIdentityMatchJudgment
		}
	}
	switch input.Status {
	case "scored":
		if input.Probability == nil || math.IsNaN(*input.Probability) ||
			math.IsInf(*input.Probability, 0) || *input.Probability < 0 ||
			*input.Probability > 1 || input.ErrorClass != "" {
			return ErrInvalidIdentityMatchJudgment
		}
	case "retryable_error", "terminal_error":
		if input.Probability != nil || !validJournalLabel(input.ErrorClass) {
			return ErrInvalidIdentityMatchJudgment
		}
	default:
		return ErrInvalidIdentityMatchJudgment
	}
	return nil
}

func randomJudgmentLeaseToken() (string, error) {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate identity match judgment token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

// ClaimNextIdentityMatchJudgmentContext walks candidate IDs after the last
// committed judgment and wraps once. Only participant pairs awaiting review
// are scored. A lease is fenced by a fresh random token and exact fingerprint.
func (s *Store) ClaimNextIdentityMatchJudgmentContext(
	ctx context.Context, owner string, leaseDuration time.Duration, scoringVersion ...string,
) (*IdentityMatchJudgmentLease, error) {
	if !validJournalLabel(owner) || leaseDuration <= 0 || leaseDuration > time.Hour || len(scoringVersion) > 1 {
		return nil, ErrInvalidIdentityMatchJudgment
	}
	version := ""
	if len(scoringVersion) == 1 {
		version = scoringVersion[0]
		if !personMatchFingerprint.MatchString(version) {
			return nil, ErrInvalidIdentityMatchJudgment
		}
	}
	var claimed *IdentityMatchJudgmentLease
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var cursor int64
		err := tx.QueryRowContext(ctx, `SELECT candidate_id FROM person_match_judgment_cursor WHERE singleton = 1`).Scan(&cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read identity match judgment cursor: %w", err)
		}
		now := time.Now().UTC()
		for _, bounds := range [][2]int64{{cursor, math.MaxInt64}, {0, cursor}} {
			if bounds[0] >= bounds[1] {
				continue
			}
			last := bounds[0]
			for {
				rows, err := tx.QueryContext(ctx, `SELECT id FROM identity_match_candidates
					WHERE state = 'candidate' AND left_kind = 'participant'
					  AND right_kind = 'participant' AND id > ? AND id <= ?
					ORDER BY id LIMIT 128`, last, bounds[1])
				if err != nil {
					return fmt.Errorf("list identity match scoring candidates: %w", err)
				}
				ids := make([]int64, 0, 128)
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						_ = rows.Close()
						return err
					}
					ids = append(ids, id)
				}
				err = rows.Err()
				_ = rows.Close()
				if err != nil {
					return err
				}
				for _, id := range ids {
					last = id
					candidate, err := getIdentityMatchCandidateTx(ctx, tx, id)
					if err != nil {
						return err
					}
					if err := populateIdentityMatchReviewContext(ctx, tx, candidate); err != nil {
						return err
					}
					workFingerprint := identityMatchScoringFingerprint(candidate.ReviewToken, version)
					var final bool
					if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
						SELECT 1 FROM person_match_judgments WHERE candidate_id = ?
						AND fingerprint = ? AND status IN ('scored', 'terminal_error')
					)`, id, workFingerprint).Scan(&final); err != nil {
						return err
					}
					if final {
						continue
					}
					var priorFingerprint string
					var priorLease, retryAfter sql.NullTime
					var attempts int
					err = tx.QueryRowContext(ctx, `SELECT fingerprint, lease_until,
						attempt_count, retry_after_at FROM person_match_judgment_work
						WHERE candidate_id = ?`, id).Scan(&priorFingerprint, &priorLease, &attempts, &retryAfter)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					if err == nil && priorFingerprint == workFingerprint &&
						((priorLease.Valid && priorLease.Time.After(now)) ||
							(retryAfter.Valid && retryAfter.Time.After(now))) {
						continue
					}
					token, err := randomJudgmentLeaseToken()
					if err != nil {
						return err
					}
					if err == nil && priorFingerprint == workFingerprint {
						attempts++
					} else {
						attempts = 1
					}
					until := now.Add(leaseDuration)
					_, err = tx.ExecContext(ctx, `INSERT INTO person_match_judgment_work
						(candidate_id, fingerprint, lease_owner, lease_token, lease_until, attempt_count, retry_after_at)
						VALUES (?, ?, ?, ?, ?, ?, NULL)
						ON CONFLICT(candidate_id) DO UPDATE SET
						fingerprint = excluded.fingerprint, lease_owner = excluded.lease_owner,
						lease_token = excluded.lease_token, lease_until = excluded.lease_until,
						attempt_count = excluded.attempt_count, retry_after_at = NULL`,
						id, workFingerprint, owner, token, until, attempts)
					if err != nil {
						return fmt.Errorf("claim identity match judgment: %w", err)
					}
					claimed = &IdentityMatchJudgmentLease{CandidateID: id,
						Fingerprint: workFingerprint, ScoringVersion: version, Candidate: *candidate,
						Owner: owner, Token: token, LeaseUntil: until}
					return nil
				}
				if len(ids) < 128 {
					break
				}
			}
		}
		return nil
	})
	return claimed, err
}

func identityMatchScoringFingerprint(reviewToken, version string) string {
	if version == "" {
		return reviewToken
	}
	sum := sha256.Sum256([]byte(reviewToken + ":" + version))
	return hex.EncodeToString(sum[:])
}

// RecordIdentityMatchJudgmentContext appends only redacted metadata. The
// journal insert, lease release, and cursor update share one transaction.
func (s *Store) RecordIdentityMatchJudgmentContext(
	ctx context.Context, lease IdentityMatchJudgmentLease, input IdentityMatchJudgmentInput,
) (*IdentityMatchJudgment, error) {
	if err := validateIdentityMatchJudgment(input); err != nil {
		return nil, err
	}
	var result *IdentityMatchJudgment
	var staleFingerprint bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var fingerprint string
		var owner, token sql.NullString
		var until sql.NullTime
		var attempts int
		err := tx.QueryRowContext(ctx, `SELECT fingerprint, lease_owner, lease_token,
			lease_until, attempt_count FROM person_match_judgment_work WHERE candidate_id = ?`,
			lease.CandidateID).Scan(&fingerprint, &owner, &token, &until, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrIdentityMatchJudgmentLeaseStale
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if fingerprint != lease.Fingerprint || !owner.Valid || owner.String != lease.Owner ||
			!token.Valid || token.String != lease.Token || !until.Valid ||
			!until.Time.After(now) || lease.Token == "" {
			return ErrIdentityMatchJudgmentLeaseStale
		}
		candidate, err := getIdentityMatchCandidateTx(ctx, tx, lease.CandidateID)
		if err != nil {
			return err
		}
		if err := populateIdentityMatchReviewContext(ctx, tx, candidate); err != nil {
			return err
		}
		status := input.Status
		probability := input.Probability
		blockers := input.Blockers
		outcome := input.Outcome
		errorClass := input.ErrorClass
		var retryAfter *time.Time
		if identityMatchScoringFingerprint(candidate.ReviewToken, lease.ScoringVersion) != lease.Fingerprint ||
			candidate.State != IdentityMatchStateCandidate {
			status, probability, blockers, outcome, errorClass = "stale", nil, nil, "review", ""
			staleFingerprint = true
		} else if status == "retryable_error" {
			if attempts >= maxIdentityMatchJudgmentAttempts {
				status = "terminal_error"
			} else {
				delay := time.Minute << min(attempts-1, 6)
				next := now.Add(delay)
				retryAfter = &next
			}
		}
		if blockers == nil {
			blockers = []string{}
		}
		blockersJSON, err := json.Marshal(blockers)
		if err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `INSERT INTO person_match_judgments
			(candidate_id, fingerprint, model_id, question_version, policy_version,
			probability, blockers_json, outcome, status, error_class, retry_after_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, created_at`,
			lease.CandidateID, lease.Fingerprint, input.ModelID, input.QuestionVersion,
			input.PolicyVersion, probability, string(blockersJSON), outcome, status,
			nullableJournalString(errorClass), retryAfter)
		result = &IdentityMatchJudgment{CandidateID: lease.CandidateID,
			Fingerprint: lease.Fingerprint, ModelID: input.ModelID,
			QuestionVersion: input.QuestionVersion, PolicyVersion: input.PolicyVersion,
			Probability: probability, Blockers: blockers, Outcome: outcome,
			Status: status, ErrorClass: errorClass, RetryAfter: retryAfter}
		if err := row.Scan(&result.ID, &result.CreatedAt); err != nil {
			return fmt.Errorf("append identity match judgment: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE person_match_judgment_work SET lease_owner = NULL,
			lease_token = NULL, lease_until = NULL, retry_after_at = ?
			WHERE candidate_id = ? AND lease_token = ?`, retryAfter, lease.CandidateID, lease.Token)
		if err != nil {
			return fmt.Errorf("release identity match judgment lease: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_match_judgment_cursor (singleton, candidate_id)
			VALUES (1, ?) ON CONFLICT(singleton) DO UPDATE SET candidate_id = excluded.candidate_id`,
			lease.CandidateID)
		if err != nil {
			return fmt.Errorf("advance identity match judgment cursor: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if staleFingerprint {
		return result, ErrIdentityMatchJudgmentFingerprintStale
	}
	return result, nil
}

func nullableJournalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// ListIdentityMatchJudgmentsContext returns newest first, capped to avoid an
// unbounded history response. Candidate ID zero selects archive-wide history.
func (s *Store) ListIdentityMatchJudgmentsContext(
	ctx context.Context, candidateID int64, limit int, beforeID ...int64,
) ([]IdentityMatchJudgment, error) {
	if candidateID < 0 || limit < 1 || limit > 100 || len(beforeID) > 1 ||
		(len(beforeID) == 1 && beforeID[0] < 0) {
		return nil, ErrInvalidIdentityMatchJudgment
	}
	query := `SELECT id, candidate_id, fingerprint,
		model_id, question_version, policy_version, probability, blockers_json,
		outcome, status, error_class, retry_after_at, created_at
		FROM person_match_judgments`
	args := []any{}
	if candidateID > 0 {
		query += ` WHERE candidate_id = ?`
		args = append(args, candidateID)
	}
	if len(beforeID) == 1 && beforeID[0] > 0 {
		if candidateID > 0 {
			query += ` AND id < ?`
		} else {
			query += ` WHERE id < ?`
		}
		args = append(args, beforeID[0])
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list identity match judgments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]IdentityMatchJudgment, 0)
	for rows.Next() {
		var judgment IdentityMatchJudgment
		var probability sql.NullFloat64
		var blockersJSON string
		var errorClass sql.NullString
		var retryAfter sql.NullTime
		if err := rows.Scan(&judgment.ID, &judgment.CandidateID, &judgment.Fingerprint,
			&judgment.ModelID, &judgment.QuestionVersion, &judgment.PolicyVersion,
			&probability, &blockersJSON, &judgment.Outcome, &judgment.Status,
			&errorClass, &retryAfter, &judgment.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(blockersJSON), &judgment.Blockers); err != nil {
			return nil, fmt.Errorf("decode identity match judgment blockers: %w", err)
		}
		if probability.Valid {
			judgment.Probability = &probability.Float64
		}
		if errorClass.Valid {
			judgment.ErrorClass = errorClass.String
		}
		if retryAfter.Valid {
			judgment.RetryAfter = &retryAfter.Time
		}
		result = append(result, judgment)
	}
	return result, rows.Err()
}
