package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	ErrEmailFamilyChanged         = errors.New("email family changed")
	ErrEmailFamilyRequestConflict = errors.New("email family request key reused with different content")
	ErrEmailFamilyTruncated       = errors.New("email family is truncated")
	ErrEmailFamilyDecisionInvalid = errors.New("invalid email family decision")
)

const emailFamilyDecisionEdgeLimit = 2000

var familyRequestKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type EmailFamilyDecisionInput struct {
	RequestKey             string  `json:"request_key"`
	Kind                   string  `json:"kind"`
	MessageIDs             []int64 `json:"message_ids"`
	ExpectedFamilyRevision string  `json:"expected_family_revision,omitzero"`
	ActorKind              string  `json:"actor_kind"`
	ActorRef               string  `json:"actor_ref,omitzero"`
	SourceIDs              []int64 `json:"source_ids,omitzero"`
}

type EmailFamilyDecision struct {
	ID                  int64      `json:"id"`
	RequestKey          string     `json:"request_key"`
	Kind                string     `json:"kind"`
	State               string     `json:"state"`
	Revision            int64      `json:"revision"`
	RequestSHA256       string     `json:"request_sha256"`
	ExpectedMemberCount int        `json:"expected_member_count"`
	ActorKind           string     `json:"actor_kind"`
	ActorRef            string     `json:"actor_ref"`
	CreatedAt           time.Time  `json:"created_at"`
	UndoneAt            *time.Time `json:"undone_at,omitempty"`
	MessageIDs          []int64    `json:"message_ids"`
}

type EmailFamilyDecisionPreview struct {
	BeforeRevision string       `json:"before_revision"`
	Family         *EmailFamily `json:"family"`
}
type EmailFamilyDecisionResult struct {
	Decision EmailFamilyDecision `json:"decision"`
	Family   *EmailFamily        `json:"family"`
}

func canonicalEmailFamilyDecision(input EmailFamilyDecisionInput) (EmailFamilyDecisionInput, string, error) {
	if len(input.MessageIDs) > 100 || len(input.SourceIDs) > 100 {
		return input, "", ErrEmailFamilyDecisionInvalid
	}
	input.MessageIDs = slices.Clone(input.MessageIDs)
	slices.Sort(input.MessageIDs)
	input.MessageIDs = slices.Compact(input.MessageIDs)
	minimum := 1
	if input.Kind == "link" {
		minimum = 2
	}
	if (input.Kind != "link" && input.Kind != "handled") || len(input.MessageIDs) < minimum || len(input.MessageIDs) > 100 || input.MessageIDs[0] < 1 || !familyRequestKeyPattern.MatchString(input.RequestKey) || len(input.ActorRef) > 256 {
		return input, "", ErrEmailFamilyDecisionInvalid
	}
	if input.ActorKind == "" {
		input.ActorKind = "owner"
	}
	if !slices.Contains([]string{"owner", "cli", "mcp", "agent"}, input.ActorKind) {
		return input, "", ErrEmailFamilyDecisionInvalid
	}
	canonical, err := json.Marshal(struct {
		Kind string
		IDs  []int64
	}{input.Kind, input.MessageIDs})
	if err != nil {
		return input, "", err
	}
	return input, fmt.Sprintf("%x", sha256.Sum256(canonical)), nil
}

// withEmailFamilyDecisionTx serializes against every evidence writer before
// reading evidence. Read committed takes fresh statement snapshots after the
// gate wait; the held exclusive gate then excludes every evidence writer. A
// serializable snapshot taken before waiting would miss shared-gate writers.
func (s *Store) withEmailFamilyDecisionTx(ctx context.Context, fn func(context.Context, *loggedTx) error) error {
	for attempt := range 5 {
		err := s.withAttributionTxOptionsContext(WithoutReadSnapshotContext(ctx), attributionLock{Exclusive: true}, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, func(tx *loggedTx) error {
			bound := context.WithValue(ctx, readSnapshotKey{}, &readSnapshot{db: s.db.DB, tx: tx})
			return fn(bound, tx)
		})
		if err == nil || attempt == 4 || !dailyNoteRetryable(ctx, s, err) {
			return err
		}
	}
	return errors.New("email family decision transaction attempts exhausted")
}

func (s *Store) PreviewEmailFamilyDecisionContext(ctx context.Context, input EmailFamilyDecisionInput) (*EmailFamilyDecisionPreview, error) {
	input, _, err := canonicalEmailFamilyDecision(input)
	if err != nil {
		return nil, err
	}
	ctx = emailFamilyScope(ctx, input.SourceIDs)
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	before, err := s.emailFamilySnapshotIDs(ctx, input.MessageIDs, nil)
	if err != nil {
		return nil, err
	}
	projected := EmailFamilyDecision{Kind: input.Kind, State: "active", Revision: 1, ExpectedMemberCount: len(input.MessageIDs), MessageIDs: input.MessageIDs}
	after, err := s.emailFamilySnapshotIDs(ctx, input.MessageIDs, &projected)
	if err != nil {
		return nil, err
	}
	return &EmailFamilyDecisionPreview{BeforeRevision: before.Revision, Family: after}, nil
}

func (s *Store) ApplyEmailFamilyDecisionContext(ctx context.Context, input EmailFamilyDecisionInput) (*EmailFamilyDecisionResult, error) {
	input, digest, err := canonicalEmailFamilyDecision(input)
	if err != nil {
		return nil, err
	}
	ctx = emailFamilyScope(ctx, input.SourceIDs)
	var result *EmailFamilyDecisionResult
	err = s.withEmailFamilyDecisionTx(ctx, func(ctx context.Context, tx *loggedTx) error {
		saved, err := s.emailFamilyDecisionByRequest(ctx, input.RequestKey)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if saved != nil {
			if len(saved.MessageIDs) == 0 && hasSourceScope(ctx) {
				return ErrNotFound
			}
			// Check surviving endpoints and any original endpoints still in the
			// archive before revealing a replay. Cascaded originals may be absent.
			for _, ids := range [][]int64{input.MessageIDs, saved.MessageIDs} {
				if err := s.authorizeEmailFamilyEndpoints(ctx, ids, false); err != nil {
					return err
				}
			}
			if saved.RequestSHA256 != digest {
				return ErrEmailFamilyRequestConflict
			}
			family, err := s.emailFamilySurvivingUnion(ctx, saved.MessageIDs)
			if err != nil {
				return err
			}
			result = &EmailFamilyDecisionResult{Decision: *saved, Family: family}
			return nil
		}
		if err := s.lockEmailFamilyTargets(ctx, tx, input.MessageIDs); err != nil {
			return err
		}
		family, err := s.emailFamilySnapshotIDs(ctx, input.MessageIDs, nil)
		if err != nil {
			return err
		}
		if family.Truncated {
			return ErrEmailFamilyTruncated
		}
		if input.ExpectedFamilyRevision == "" || family.Revision != input.ExpectedFamilyRevision {
			return ErrEmailFamilyChanged
		}
		projected := EmailFamilyDecision{Kind: input.Kind, State: "active", Revision: 1, ExpectedMemberCount: len(input.MessageIDs), MessageIDs: input.MessageIDs}
		projectedFamily, err := s.emailFamilySnapshotIDs(ctx, input.MessageIDs, &projected)
		if err != nil {
			return err
		}
		if projectedFamily.Truncated {
			return ErrEmailFamilyTruncated
		}
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO email_family_decisions(request_key,kind,request_sha256,expected_member_count,actor_kind,actor_ref) VALUES(?,?,?,?,?,?) RETURNING id`, input.RequestKey, input.Kind, digest, len(input.MessageIDs), input.ActorKind, input.ActorRef).Scan(&id)
		if err != nil {
			return err
		}
		for _, messageID := range input.MessageIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO email_family_decision_members(decision_id,message_id) VALUES(?,?)`, id, messageID); err != nil {
				return err
			}
		}
		saved, err = s.emailFamilyDecision(ctx, id)
		if err != nil {
			return err
		}
		family, err = s.emailFamilySnapshotIDs(ctx, input.MessageIDs, nil)
		if err != nil {
			return err
		}
		if family.Truncated {
			return ErrEmailFamilyTruncated
		}
		result = &EmailFamilyDecisionResult{Decision: *saved, Family: family}
		return nil
	})
	return result, err
}

func (s *Store) UndoEmailFamilyDecisionContext(ctx context.Context, id, expectedRevision int64, expectedFamilyRevision string, options EmailFamilyOptions) (*EmailFamilyDecisionResult, error) {
	ctx = emailFamilyScope(ctx, options.SourceIDs)
	var result *EmailFamilyDecisionResult
	err := s.withEmailFamilyDecisionTx(ctx, func(ctx context.Context, tx *loggedTx) error {
		decision, err := s.emailFamilyDecision(ctx, id)
		if err != nil {
			return err
		}
		if err := s.authorizeEmailFamilyEndpoints(ctx, decision.MessageIDs, false); err != nil {
			return err
		}
		// A fully cascaded decision is visible only without a narrowed scope.
		if len(decision.MessageIDs) == 0 && hasSourceScope(ctx) {
			return ErrNotFound
		}
		family, err := s.emailFamilySurvivingUnion(ctx, decision.MessageIDs)
		if err != nil {
			return err
		}
		if decision.State == "undone" {
			result = &EmailFamilyDecisionResult{Decision: *decision, Family: family}
			return nil
		}
		if family.Truncated {
			return ErrEmailFamilyTruncated
		}
		if decision.Revision != expectedRevision || family.Revision != expectedFamilyRevision || expectedFamilyRevision == "" {
			return ErrEmailFamilyChanged
		}
		if _, err := tx.ExecContext(ctx, `UPDATE email_family_decisions SET state='undone',revision=revision+1,undone_at=CURRENT_TIMESTAMP WHERE id=?`, id); err != nil {
			return err
		}
		decision, err = s.emailFamilyDecision(ctx, id)
		if err != nil {
			return err
		}
		family, err = s.emailFamilySurvivingUnion(ctx, decision.MessageIDs)
		if err != nil {
			return err
		}
		result = &EmailFamilyDecisionResult{Decision: *decision, Family: family}
		return nil
	})
	return result, err
}

func (s *Store) lockEmailFamilyTargets(ctx context.Context, tx *loggedTx, ids []int64) error {
	for _, id := range ids {
		if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
			return err
		}
	}
	return s.authorizeEmailFamilyEndpoints(ctx, ids, true)
}

func hasSourceScope(ctx context.Context) bool {
	scope, _ := sourceScopeClause(ctx, "source_id")
	return scope != ""
}

func (s *Store) authorizeEmailFamilyEndpoints(ctx context.Context, ids []int64, live bool) error {
	scope, args := sourceScopeClause(ctx, "m.source_id")
	for _, id := range ids {
		var exists, visible bool
		suffix := ""
		if live {
			suffix = " AND m.deleted_at IS NULL AND COALESCE(m.message_type,'') IN ('','email')"
		}
		queryArgs := append([]any{id, id}, args...)
		err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE id=?),EXISTS(SELECT 1 FROM messages m WHERE m.id=?`+suffix+scope+`)`, queryArgs...).Scan(&exists, &visible)
		if err != nil {
			return err
		}
		if !visible && (exists || live) {
			return ErrNotFound
		}
	}
	return nil
}

const emailFamilyDecisionColumns = `id,request_key,kind,state,revision,request_sha256,expected_member_count,actor_kind,actor_ref,created_at,undone_at`

func (s *Store) emailFamilyDecisionByRequest(ctx context.Context, key string) (*EmailFamilyDecision, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM email_family_decisions WHERE request_key=?`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.emailFamilyDecision(ctx, id)
}
func (s *Store) emailFamilyDecision(ctx context.Context, id int64) (*EmailFamilyDecision, error) {
	d := &EmailFamilyDecision{MessageIDs: []int64{}}
	var undone sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT `+emailFamilyDecisionColumns+` FROM email_family_decisions WHERE id=?`, id).Scan(&d.ID, &d.RequestKey, &d.Kind, &d.State, &d.Revision, &d.RequestSHA256, &d.ExpectedMemberCount, &d.ActorKind, &d.ActorRef, &d.CreatedAt, &undone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if undone.Valid {
		d.UndoneAt = &undone.Time
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id FROM email_family_decision_members WHERE decision_id=? ORDER BY message_id LIMIT 101`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var member int64
		if err := rows.Scan(&member); err != nil {
			return nil, err
		}
		d.MessageIDs = append(d.MessageIDs, member)
	}
	if len(d.MessageIDs) > 100 {
		return nil, ErrEmailFamilyDecisionInvalid
	}
	return d, rows.Err()
}

func (s *Store) emailFamilySurvivingUnion(ctx context.Context, ids []int64) (*EmailFamily, error) {
	scope, args := sourceScopeClause(ctx, "m.source_id")
	live := []int64{}
	for _, id := range ids {
		var found bool
		queryArgs := append([]any{id}, args...)
		err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m WHERE m.id=? AND m.deleted_at IS NULL AND COALESCE(m.message_type,'') IN ('','email')`+scope+`)`, queryArgs...).Scan(&found)
		if err != nil {
			return nil, err
		}
		if found {
			live = append(live, id)
		}
	}
	if len(live) == 0 {
		return &EmailFamily{Members: []EmailFamilyMember{}, Decisions: []EmailFamilyDecision{}, Revision: fmt.Sprintf("%x", sha256.Sum256([]byte("email-family-empty-v1")))}, nil
	}
	return s.emailFamilySnapshotIDs(ctx, live, nil)
}

// GetEmailFamilyDecisionContext reads the decision and its current surviving
// endpoint union in one snapshot, including an empty union after deletion.
func (s *Store) GetEmailFamilyDecisionContext(ctx context.Context, id int64, options EmailFamilyOptions) (*EmailFamilyDecisionResult, error) {
	ctx = emailFamilyScope(ctx, options.SourceIDs)
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	decision, err := s.emailFamilyDecision(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeEmailFamilyEndpoints(ctx, decision.MessageIDs, false); err != nil {
		return nil, err
	}
	if len(decision.MessageIDs) == 0 && hasSourceScope(ctx) {
		return nil, ErrNotFound
	}
	family, err := s.emailFamilySurvivingUnion(ctx, decision.MessageIDs)
	if err != nil {
		return nil, err
	}
	return &EmailFamilyDecisionResult{Decision: *decision, Family: family}, nil
}

// ListEmailFamilyDecisionsContext returns only history whose surviving
// endpoints are all in scope. The seed must still be a live visible email.
func (s *Store) ListEmailFamilyDecisionsContext(ctx context.Context, id int64, options EmailFamilyOptions) ([]EmailFamilyDecision, error) {
	ctx = emailFamilyScope(ctx, options.SourceIDs)
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.authorizeEmailFamilyEndpoints(ctx, []int64{id}, true); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT decision_id FROM email_family_decision_members WHERE message_id=? ORDER BY decision_id DESC LIMIT 20`, id)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	out := []EmailFamilyDecision{}
	for _, id := range ids {
		d, err := s.emailFamilyDecision(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := s.authorizeEmailFamilyEndpoints(ctx, d.MessageIDs, false); errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, nil
}

// Read at most the remaining edge budget plus one detection row. Excluding
// already visited decisions prevents repeated fan-out at every member.
func (s *Store) emailFamilyEdges(ctx context.Context, id int64, seen map[int64]bool, budget int) ([]EmailFamilyDecision, int, bool, error) {
	args := []any{id}
	exclude := ""
	if len(seen) > 0 {
		ids := make([]int64, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		exclude = " AND d.id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	}
	args = append(args, budget+1)
	rows, err := s.db.QueryContext(ctx, `SELECT d.id,d.kind,d.revision,d.expected_member_count,dm.message_id FROM email_family_decisions d JOIN email_family_decision_members anchor ON anchor.decision_id=d.id JOIN email_family_decision_members dm ON dm.decision_id=d.id WHERE anchor.message_id=? AND d.state='active'`+exclude+` ORDER BY d.id,dm.message_id LIMIT ?`, args...)
	if err != nil {
		return nil, 0, false, err
	}
	decisions := []EmailFamilyDecision{}
	count := 0
	for rows.Next() {
		var decision EmailFamilyDecision
		var member int64
		if err := rows.Scan(&decision.ID, &decision.Kind, &decision.Revision, &decision.ExpectedMemberCount, &member); err != nil {
			_ = rows.Close()
			return nil, count, false, err
		}
		count++
		if count > budget {
			break
		}
		if len(decisions) == 0 || decisions[len(decisions)-1].ID != decision.ID {
			decision.State = "active"
			decision.MessageIDs = []int64{}
			decisions = append(decisions, decision)
		}
		last := &decisions[len(decisions)-1]
		last.MessageIDs = append(last.MessageIDs, member)
	}
	err = rows.Err()
	_ = rows.Close()
	return decisions, count, count > budget, err
}

// Retargeting preserves any deficit already caused by cascades. Only an
// observed collision reduces the original expected population.
func (s *Store) retargetEmailFamilyDecisions(ctx context.Context, tx *loggedTx, survivor, duplicate int64) error {
	if survivor == duplicate {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT decision_id FROM email_family_decision_members WHERE message_id=? ORDER BY decision_id`, duplicate)
	if err != nil {
		return err
	}
	ids := []int64{}
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
		var collision bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM email_family_decision_members WHERE decision_id=? AND message_id=?)`, id, survivor).Scan(&collision); err != nil {
			return err
		}
		if collision {
			if _, err := tx.ExecContext(ctx, `DELETE FROM email_family_decision_members WHERE decision_id=? AND message_id=?`, id, duplicate); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE email_family_decisions SET expected_member_count=expected_member_count-1,revision=revision+1 WHERE id=?`, id); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE email_family_decision_members SET message_id=? WHERE decision_id=? AND message_id=?`, survivor, id, duplicate); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE email_family_decisions SET revision=revision+1 WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}
