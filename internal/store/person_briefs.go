package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrPersonBriefNotTracked reports that brief enrollment was refused
	// because the person has no person_tracking row.
	ErrPersonBriefNotTracked = errors.New("person is not tracked")
	// ErrPersonBriefNotFound reports that the requested brief version does not
	// exist, including the case where no version is current.
	ErrPersonBriefNotFound = errors.New("person brief not found")
)

// Brief version statuses. At most one version per person is current; a
// regeneration supersedes it, and the owner can reject it.
const (
	PersonBriefStatusCurrent    = "current"
	PersonBriefStatusSuperseded = "superseded"
	PersonBriefStatusRejected   = "rejected"
)

const (
	maxPersonBriefEnrollmentList = 1_000
	maxPersonBriefVersionList    = 200
)

// PersonBriefNotTrackedError names the command that makes enrollment possible.
// Callers can match it with errors.Is(err, ErrPersonBriefNotTracked).
type PersonBriefNotTrackedError struct {
	PersonID int64
}

func (e *PersonBriefNotTrackedError) Error() string {
	return fmt.Sprintf(
		"person %d must be tracked before brief enrollment: run `msgvault person track %d` first, or enroll with --track",
		e.PersonID, e.PersonID)
}

func (e *PersonBriefNotTrackedError) Unwrap() error { return ErrPersonBriefNotTracked }

// PersonBriefEnrollment reports whether a person is opted in to generated
// briefs. An absent person_brief_enrollments row means not enrolled.
type PersonBriefEnrollment struct {
	PersonID  int64      `json:"person_id"`
	Enrolled  bool       `json:"enrolled"`
	EnabledAt *time.Time `json:"enabled_at"`
	Actor     string     `json:"actor"`
}

// PersonBrief is one immutable dated brief version. Structured and RenderedText
// are written once and never updated.
type PersonBrief struct {
	ID                        int64           `json:"id"`
	PersonID                  int64           `json:"person_id"`
	Version                   int             `json:"version"`
	GenerationID              int64           `json:"generation_id"`
	Status                    string          `json:"status"`
	ProgramID                 string          `json:"program_id"`
	ProgramVersion            string          `json:"program_version"`
	ProgramFingerprint        string          `json:"program_fingerprint"`
	Provider                  string          `json:"provider"`
	ProviderVersion           string          `json:"provider_version"`
	Model                     string          `json:"model"`
	ModelVersion              string          `json:"model_version"`
	ProviderPolicyFingerprint string          `json:"provider_policy_fingerprint"`
	Boundary                  json.RawMessage `json:"boundary_json"`
	Structured                json.RawMessage `json:"structured_json"`
	RenderedText              string          `json:"rendered_text"`
	RendererPolicy            string          `json:"renderer_policy"`
	DroppedItemCount          int             `json:"dropped_item_count"`
	GeneratedAt               time.Time       `json:"generated_at"`
	SupersededAt              *time.Time      `json:"superseded_at"`
	RejectedAt                *time.Time      `json:"rejected_at"`
	RejectedReason            string          `json:"rejected_reason"`
	CreatedAt                 time.Time       `json:"created_at"`
}

// PersonBriefInsert is one generated version, ready to be written inside the
// sweep's apply transaction. EvidenceIDs are stored in the order given; that
// order is the pointer ordinal a reader sees.
type PersonBriefInsert struct {
	PersonID                  int64
	GenerationID              int64
	ProgramID                 string
	ProgramVersion            string
	ProgramFingerprint        string
	Provider                  string
	ProviderVersion           string
	Model                     string
	ModelVersion              string
	ProviderPolicyFingerprint string
	Boundary                  json.RawMessage
	Structured                json.RawMessage
	RenderedText              string
	RendererPolicy            string
	DroppedItemCount          int
	GeneratedAt               time.Time
	EvidenceIDs               []int64
}

// PersonBriefEvidencePointer is one cited archive item. Supported is false once
// a later status event invalidated the evidence's source; the reader shows the
// item as unsupported rather than hiding it.
type PersonBriefEvidencePointer struct {
	Ordinal     int       `json:"ordinal"`
	EvidenceID  int64     `json:"evidence_id"`
	EvidenceKey string    `json:"evidence_key"`
	SourceRef   string    `json:"source_ref"`
	SourceURL   string    `json:"source_url"`
	Directness  string    `json:"directness"`
	EventTime   time.Time `json:"event_time"`
	Supported   bool      `json:"evidence_supported"`
}

// PersonBriefEligibility is one enrolled and tracked person with the metadata a
// scheduler needs to decide whether a new brief is due. Version is zero when the
// person has no brief yet; otherwise the fields describe the latest version,
// whose status is current unless the owner rejected it.
type PersonBriefEligibility struct {
	PersonID        int64      `json:"person_id"`
	EnabledAt       time.Time  `json:"enabled_at"`
	Version         int        `json:"version"`
	Status          string     `json:"status"`
	GeneratedAt     *time.Time `json:"generated_at"`
	ThroughSequence int64      `json:"through_sequence"`
}

const personBriefColumns = `id, person_id, version, generation_id, status, program_id,
	program_version, program_fingerprint, provider, provider_version, model,
	model_version, provider_policy_fingerprint, boundary_json, structured_json,
	rendered_text, renderer_policy, dropped_item_count, generated_at,
	superseded_at, rejected_at, rejected_reason, created_at`

// GetPersonBriefEnrollmentContext reads the explicit enrollment state.
func (s *Store) GetPersonBriefEnrollmentContext(
	ctx context.Context, personID int64,
) (*PersonBriefEnrollment, error) {
	var enrollment *PersonBriefEnrollment
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		enrollment, err = s.getPersonBriefEnrollmentTx(ctx, tx, personID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return enrollment, nil
}

// SetPersonBriefEnrollmentContext enrolls or unenrolls a person idempotently.
// Enrolling requires a tracking row; track creates one in the same transaction
// so enrollment never outlives the tracking it depends on. Unenrolling deletes
// the row and leaves existing versions readable.
func (s *Store) SetPersonBriefEnrollmentContext(
	ctx context.Context, personID int64, enabled bool, actor string, track bool,
) (*PersonBriefEnrollment, error) {
	actor = strings.TrimSpace(actor)
	if enabled && actor == "" {
		return nil, errors.New("set person brief enrollment: actor is required")
	}
	var enrollment *PersonBriefEnrollment
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if !enabled {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM person_brief_enrollments WHERE person_id = ?`, personID); err != nil {
				return fmt.Errorf("unenroll person %d from briefs: %w", personID, err)
			}
			var err error
			enrollment, err = s.getPersonBriefEnrollmentTx(ctx, tx, personID)
			return err
		}
		tracking, err := s.getPersonTrackingTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		if !tracking.Tracked {
			if !track {
				return &PersonBriefNotTrackedError{PersonID: personID}
			}
			if _, err := s.setPersonTrackingTx(ctx, tx, personID, true); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO person_brief_enrollments (person_id, enabled_at, actor)
			VALUES (?, ?, ?)
			ON CONFLICT (person_id) DO NOTHING
		`, personID, time.Now().UTC(), actor); err != nil {
			return fmt.Errorf("enroll person %d in briefs: %w", personID, err)
		}
		enrollment, err = s.getPersonBriefEnrollmentTx(ctx, tx, personID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return enrollment, nil
}

// ListPersonBriefEnrollmentsContext returns a bounded ascending page of enrolled
// people.
func (s *Store) ListPersonBriefEnrollmentsContext(
	ctx context.Context, afterID int64, limit int,
) ([]PersonBriefEnrollment, error) {
	if limit < 1 || limit > maxPersonBriefEnrollmentList {
		return nil, fmt.Errorf(
			"list person brief enrollments: limit must be between 1 and %d",
			maxPersonBriefEnrollmentList)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT person_id, enabled_at, actor FROM person_brief_enrollments
		WHERE person_id > ?
		ORDER BY person_id
		LIMIT ?`), afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list person brief enrollments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	enrollments := make([]PersonBriefEnrollment, 0, limit)
	for rows.Next() {
		enrollment := PersonBriefEnrollment{Enrolled: true}
		var enabledAt nullableTimestamp
		if err := rows.Scan(&enrollment.PersonID, &enabledAt, &enrollment.Actor); err != nil {
			return nil, fmt.Errorf("scan person brief enrollment: %w", err)
		}
		if enabledAt.Valid {
			enrollment.EnabledAt = &enabledAt.Time
		}
		enrollments = append(enrollments, enrollment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person brief enrollments: %w", err)
	}
	return enrollments, nil
}

// ListBriefEligiblePeopleContext returns a bounded ascending page of people that
// are both enrolled and tracked, each with the latest version's metadata.
func (s *Store) ListBriefEligiblePeopleContext(
	ctx context.Context, afterID int64, limit int,
) ([]PersonBriefEligibility, error) {
	if limit < 1 || limit > maxPersonBriefEnrollmentList {
		return nil, fmt.Errorf(
			"list brief eligible people: limit must be between 1 and %d",
			maxPersonBriefEnrollmentList)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT enrollment.person_id, enrollment.enabled_at,
		       latest.version, latest.status, latest.generated_at, latest.boundary_json
		FROM person_brief_enrollments enrollment
		LEFT JOIN person_briefs latest
		  ON latest.person_id = enrollment.person_id
		 AND latest.version = (
			SELECT MAX(candidate.version) FROM person_briefs candidate
			WHERE candidate.person_id = enrollment.person_id)
		WHERE enrollment.person_id > ?
		  AND EXISTS (
			SELECT 1 FROM person_tracking tracking
			WHERE tracking.person_id = enrollment.person_id)
		ORDER BY enrollment.person_id
		LIMIT ?`), afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list brief eligible people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	eligible := make([]PersonBriefEligibility, 0, limit)
	for rows.Next() {
		var (
			item        PersonBriefEligibility
			enabledAt   nullableTimestamp
			version     sql.NullInt64
			status      sql.NullString
			generatedAt nullableTimestamp
			boundary    sql.NullString
		)
		if err := rows.Scan(&item.PersonID, &enabledAt, &version, &status,
			&generatedAt, &boundary); err != nil {
			return nil, fmt.Errorf("scan brief eligible person: %w", err)
		}
		if enabledAt.Valid {
			item.EnabledAt = enabledAt.Time
		}
		item.Version = int(version.Int64)
		item.Status = status.String
		if generatedAt.Valid {
			item.GeneratedAt = &generatedAt.Time
		}
		if boundary.Valid {
			sequence, err := personBriefThroughSequence(json.RawMessage(boundary.String))
			if err != nil {
				return nil, fmt.Errorf("person %d: %w", item.PersonID, err)
			}
			item.ThroughSequence = sequence
		}
		eligible = append(eligible, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate brief eligible people: %w", err)
	}
	return eligible, nil
}

// GetPersonBriefContext reads one version. Version 0 means the current version.
func (s *Store) GetPersonBriefContext(
	ctx context.Context, personID int64, version int,
) (*PersonBrief, error) {
	if version < 0 {
		return nil, errors.New("get person brief: version must not be negative")
	}
	predicate, arg := `status = ?`, any(PersonBriefStatusCurrent)
	if version > 0 {
		predicate, arg = `version = ?`, any(version)
	}
	row := s.db.QueryRowContext(ctx, s.Rebind(`SELECT `+personBriefColumns+`
		FROM person_briefs WHERE person_id = ? AND `+predicate), personID, arg)
	brief, err := scanPersonBrief(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("person %d brief: %w", personID, ErrPersonBriefNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &brief, nil
}

// ListPersonBriefVersionsContext returns a bounded newest-first version history.
func (s *Store) ListPersonBriefVersionsContext(
	ctx context.Context, personID int64, limit int,
) ([]PersonBrief, error) {
	if limit < 1 || limit > maxPersonBriefVersionList {
		return nil, fmt.Errorf("list person brief versions: limit must be between 1 and %d",
			maxPersonBriefVersionList)
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT `+personBriefColumns+`
		FROM person_briefs WHERE person_id = ?
		ORDER BY version DESC
		LIMIT ?`), personID, limit)
	if err != nil {
		return nil, fmt.Errorf("list person brief versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	briefs := make([]PersonBrief, 0, limit)
	for rows.Next() {
		brief, err := scanPersonBrief(rows)
		if err != nil {
			return nil, err
		}
		briefs = append(briefs, brief)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person brief versions: %w", err)
	}
	return briefs, nil
}

// ListPersonBriefEvidenceContext returns the version's cited archive items in
// ordinal order, each carrying the support the latest status event reports.
func (s *Store) ListPersonBriefEvidenceContext(
	ctx context.Context, briefID int64,
) ([]PersonBriefEvidencePointer, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
		SELECT pointer.ordinal, evidence.id, evidence.evidence_key, evidence.source_ref,
		       evidence.source_url, evidence.directness, evidence.event_time,
		       (SELECT event.supported
		        FROM person_fact_evidence_status_events event
		        JOIN person_fact_generations generation ON generation.id = event.generation_id
		        WHERE event.person_id = evidence.person_id
		          AND event.evidence_key = evidence.evidence_key
		          AND event.source_version = evidence.source_version
		        ORDER BY generation.resolved_at DESC, event.id DESC
		        LIMIT 1)
		FROM person_brief_evidence pointer
		JOIN person_fact_evidence evidence ON evidence.id = pointer.evidence_id
		WHERE pointer.brief_id = ?
		ORDER BY pointer.ordinal`), briefID)
	if err != nil {
		return nil, fmt.Errorf("list person brief evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pointers := make([]PersonBriefEvidencePointer, 0)
	for rows.Next() {
		var (
			pointer   PersonBriefEvidencePointer
			eventTime nullableTimestamp
			supported sql.NullBool
		)
		if err := rows.Scan(&pointer.Ordinal, &pointer.EvidenceID, &pointer.EvidenceKey,
			&pointer.SourceRef, &pointer.SourceURL, &pointer.Directness,
			&eventTime, &supported); err != nil {
			return nil, fmt.Errorf("scan person brief evidence: %w", err)
		}
		if eventTime.Valid {
			pointer.EventTime = eventTime.Time
		}
		// No status event means nothing has invalidated the source yet.
		pointer.Supported = !supported.Valid || supported.Bool
		pointers = append(pointers, pointer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person brief evidence: %w", err)
	}
	return pointers, nil
}

// RejectPersonBriefContext rejects the current version. The row keeps its
// structure and rendered text; only the owner's verdict is recorded, and the
// next regeneration produces the next version.
func (s *Store) RejectPersonBriefContext(
	ctx context.Context, personID int64, reason string, at time.Time,
) (*PersonBrief, error) {
	if at.IsZero() {
		return nil, errors.New("reject person brief: rejection time is required")
	}
	var brief *PersonBrief
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, `UPDATE person_briefs
			SET status = ?, rejected_at = ?, rejected_reason = ?
			WHERE person_id = ? AND status = ?`,
			PersonBriefStatusRejected, at.UTC(), strings.TrimSpace(reason),
			personID, PersonBriefStatusCurrent)
		if err != nil {
			return fmt.Errorf("reject person %d brief: %w", personID, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("reject person %d brief: %w", personID, err)
		}
		if changed == 0 {
			return fmt.Errorf("person %d has no current brief: %w", personID, ErrPersonBriefNotFound)
		}
		row := tx.QueryRowContext(ctx, `SELECT `+personBriefColumns+`
			FROM person_briefs WHERE person_id = ? AND status = ?
			ORDER BY version DESC LIMIT 1`, personID, PersonBriefStatusRejected)
		rejected, err := scanPersonBrief(row)
		if err != nil {
			return err
		}
		brief = &rejected
		return nil
	})
	if err != nil {
		return nil, err
	}
	return brief, nil
}

// applyPersonBriefTx writes one generated version inside a caller's
// transaction, so a sweep commits the brief with the generation, claims, and
// cursor advances it was produced from. It supersedes the previous current
// version and records the evidence pointers in the order given.
//
// Enrollment is deliberately not re-checked here: eligibility is decided before
// the provider call, and a concurrent unenrollment must not fail an attempt that
// already spent its budget.
func (s *Store) applyPersonBriefTx(
	ctx context.Context, tx *loggedTx, input PersonBriefInsert,
) (PersonBrief, error) {
	if err := validatePersonBriefInsert(input); err != nil {
		return PersonBrief{}, err
	}
	var generationPersonID int64
	err := tx.QueryRowContext(ctx,
		`SELECT person_id FROM person_fact_generations WHERE id = ?`,
		input.GenerationID).Scan(&generationPersonID)
	if errors.Is(err, sql.ErrNoRows) {
		return PersonBrief{}, fmt.Errorf(
			"apply person brief: generation %d does not exist", input.GenerationID)
	}
	if err != nil {
		return PersonBrief{}, fmt.Errorf("apply person brief: load generation: %w", err)
	}
	if generationPersonID != input.PersonID {
		return PersonBrief{}, fmt.Errorf(
			"apply person brief: generation %d belongs to person %d, not %d",
			input.GenerationID, generationPersonID, input.PersonID)
	}
	brief, err := s.insertPersonBriefTx(ctx, tx, input)
	if err != nil {
		return PersonBrief{}, err
	}
	if err := s.insertPersonBriefEvidenceTx(ctx, tx, brief, input.EvidenceIDs); err != nil {
		return PersonBrief{}, err
	}
	return brief, nil
}

// insertPersonBriefTx appends the next version and supersedes the previous
// current one. Supersession runs first so the one-current index never sees two.
func (s *Store) insertPersonBriefTx(
	ctx context.Context, tx *loggedTx, input PersonBriefInsert,
) (PersonBrief, error) {
	generatedAt := input.GeneratedAt.UTC()
	var nextVersion int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM person_briefs WHERE person_id = ?`,
		input.PersonID).Scan(&nextVersion); err != nil {
		return PersonBrief{}, fmt.Errorf("read next person %d brief version: %w", input.PersonID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_briefs
		SET status = ?, superseded_at = ?
		WHERE person_id = ? AND status = ?`,
		PersonBriefStatusSuperseded, generatedAt, input.PersonID,
		PersonBriefStatusCurrent); err != nil {
		return PersonBrief{}, fmt.Errorf("supersede person %d brief: %w", input.PersonID, err)
	}
	var briefID int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO person_briefs
			(person_id, version, generation_id, status, program_id, program_version,
			 program_fingerprint, provider, provider_version, model, model_version,
			 provider_policy_fingerprint, boundary_json, structured_json, rendered_text,
			 renderer_policy, dropped_item_count, generated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+
		s.dialect.JSONBindExpr()+`, `+s.dialect.JSONBindExpr()+`, ?, ?, ?, ?)
		RETURNING id`,
		input.PersonID, nextVersion, input.GenerationID, PersonBriefStatusCurrent,
		input.ProgramID, input.ProgramVersion, input.ProgramFingerprint, input.Provider,
		input.ProviderVersion, input.Model, input.ModelVersion,
		input.ProviderPolicyFingerprint, string(input.Boundary), string(input.Structured),
		input.RenderedText, input.RendererPolicy, input.DroppedItemCount, generatedAt,
	).Scan(&briefID); err != nil {
		return PersonBrief{}, fmt.Errorf("insert person %d brief: %w", input.PersonID, err)
	}
	row := tx.QueryRowContext(ctx, `SELECT `+personBriefColumns+`
		FROM person_briefs WHERE id = ?`, briefID)
	return scanPersonBrief(row)
}

func (s *Store) insertPersonBriefEvidenceTx(
	ctx context.Context, tx *loggedTx, brief PersonBrief, evidenceIDs []int64,
) error {
	for ordinal, evidenceID := range evidenceIDs {
		var evidencePersonID int64
		err := tx.QueryRowContext(ctx,
			`SELECT person_id FROM person_fact_evidence WHERE id = ?`,
			evidenceID).Scan(&evidencePersonID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("apply person brief: evidence %d does not exist", evidenceID)
		}
		if err != nil {
			return fmt.Errorf("apply person brief: load evidence %d: %w", evidenceID, err)
		}
		if evidencePersonID != brief.PersonID {
			return fmt.Errorf("apply person brief: evidence %d belongs to person %d, not %d",
				evidenceID, evidencePersonID, brief.PersonID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_brief_evidence
			(brief_id, evidence_id, ordinal) VALUES (?, ?, ?)`,
			brief.ID, evidenceID, ordinal); err != nil {
			return fmt.Errorf("insert person brief evidence pointer: %w", err)
		}
	}
	return nil
}

func (s *Store) getPersonBriefEnrollmentTx(
	ctx context.Context, tx *loggedTx, personID int64,
) (*PersonBriefEnrollment, error) {
	enrollment := &PersonBriefEnrollment{PersonID: personID}
	var (
		enabledAt nullableTimestamp
		actor     sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT p.id, enrollment.enabled_at, enrollment.actor
		FROM persons p
		LEFT JOIN person_brief_enrollments enrollment ON enrollment.person_id = p.id
		WHERE p.id = ?
	`, personID).Scan(&enrollment.PersonID, &enabledAt, &actor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("get person %d brief enrollment: %w", personID, ErrPersonNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get person %d brief enrollment: %w", personID, err)
	}
	if enabledAt.Valid {
		enrollment.Enrolled = true
		enrollment.EnabledAt = &enabledAt.Time
		enrollment.Actor = actor.String
	}
	return enrollment, nil
}

func validatePersonBriefInsert(input PersonBriefInsert) error {
	if input.PersonID <= 0 || input.GenerationID <= 0 {
		return errors.New("apply person brief: person and generation are required")
	}
	if strings.TrimSpace(input.ProgramID) == "" || strings.TrimSpace(input.ProgramVersion) == "" ||
		strings.TrimSpace(input.ProgramFingerprint) == "" {
		return errors.New("apply person brief: complete program identity is required")
	}
	if strings.TrimSpace(input.Provider) == "" || strings.TrimSpace(input.ProviderVersion) == "" ||
		strings.TrimSpace(input.Model) == "" || strings.TrimSpace(input.ModelVersion) == "" ||
		strings.TrimSpace(input.ProviderPolicyFingerprint) == "" {
		return errors.New("apply person brief: complete provider identity is required")
	}
	if strings.TrimSpace(input.RendererPolicy) == "" {
		return errors.New("apply person brief: renderer policy is required")
	}
	if !json.Valid(input.Boundary) || !json.Valid(input.Structured) {
		return errors.New("apply person brief: boundary and structure must be JSON")
	}
	if input.DroppedItemCount < 0 {
		return errors.New("apply person brief: dropped item count must not be negative")
	}
	if input.GeneratedAt.IsZero() {
		return errors.New("apply person brief: generation time is required")
	}
	seen := make(map[int64]struct{}, len(input.EvidenceIDs))
	for _, evidenceID := range input.EvidenceIDs {
		if evidenceID <= 0 {
			return errors.New("apply person brief: evidence pointers must name stored evidence")
		}
		if _, duplicate := seen[evidenceID]; duplicate {
			return fmt.Errorf("apply person brief: evidence %d is cited twice", evidenceID)
		}
		seen[evidenceID] = struct{}{}
	}
	return nil
}

// personBriefThroughSequence reads the archive commit sequence the version was
// generated through. A boundary without one reports zero, which reads as "no
// bound yet" to a scheduler.
func personBriefThroughSequence(boundary json.RawMessage) (int64, error) {
	if len(boundary) == 0 {
		return 0, nil
	}
	var decoded struct {
		ThroughSequence int64 `json:"through_sequence"`
	}
	if err := json.Unmarshal(boundary, &decoded); err != nil {
		return 0, fmt.Errorf("decode person brief boundary: %w", err)
	}
	return decoded.ThroughSequence, nil
}

func scanPersonBrief(row scanner) (PersonBrief, error) {
	var (
		brief        PersonBrief
		boundary     string
		structured   string
		generatedAt  nullableTimestamp
		supersededAt nullableTimestamp
		rejectedAt   nullableTimestamp
		createdAt    nullableTimestamp
	)
	if err := row.Scan(&brief.ID, &brief.PersonID, &brief.Version, &brief.GenerationID,
		&brief.Status, &brief.ProgramID, &brief.ProgramVersion, &brief.ProgramFingerprint,
		&brief.Provider, &brief.ProviderVersion, &brief.Model, &brief.ModelVersion,
		&brief.ProviderPolicyFingerprint, &boundary, &structured, &brief.RenderedText,
		&brief.RendererPolicy, &brief.DroppedItemCount, &generatedAt, &supersededAt,
		&rejectedAt, &brief.RejectedReason, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PersonBrief{}, err
		}
		return PersonBrief{}, fmt.Errorf("scan person brief: %w", err)
	}
	brief.Boundary = json.RawMessage(boundary)
	brief.Structured = json.RawMessage(structured)
	brief.GeneratedAt = generatedAt.Time
	brief.CreatedAt = createdAt.Time
	if supersededAt.Valid {
		brief.SupersededAt = &supersededAt.Time
	}
	if rejectedAt.Valid {
		brief.RejectedAt = &rejectedAt.Time
	}
	return brief, nil
}
