package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentRestriction is an owner-configured rule. It does not yet restrict reads.
type AgentRestriction struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	SourceID  int64     `json:"source_id,omitempty"`
	LabelID   int64     `json:"label_id,omitempty"`
	PersonID  int64     `json:"person_id,omitempty"`
	Reveal    string    `json:"reveal"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

// AddAgentRestriction saves a rule targeting one existing archive object.
func (s *Store) AddAgentRestriction(ctx context.Context, r AgentRestriction) (saved AgentRestriction, retErr error) {
	if r.Reveal == "" {
		r.Reveal = "headers"
	}
	if r.Reveal != "minimal" && r.Reveal != "headers" && r.Reveal != "headers_subject" {
		return r, errors.New("invalid reveal level")
	}
	var target int64
	switch r.Kind {
	case "source":
		if r.LabelID != 0 || r.PersonID != 0 {
			return r, errors.New("source rule has extra target")
		}
		target = r.SourceID
	case "label":
		if r.SourceID != 0 || r.PersonID != 0 {
			return r, errors.New("label rule has extra target")
		}
		target = r.LabelID
	case string(AttributeObjectPerson):
		if r.SourceID != 0 || r.LabelID != 0 {
			return r, errors.New("person rule has extra target")
		}
		target = r.PersonID
	default:
		return r, errors.New("invalid restriction kind")
	}
	if target <= 0 {
		return r, errors.New("restriction target must be a positive ID")
	}
	// Removal keeps execution ownership through provider logout and the local
	// cascade. A new rule must not make removal ineligible after logout starts.
	sourceID := r.SourceID
	if r.Kind == "label" {
		if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT source_id FROM labels WHERE id=?`), r.LabelID).Scan(&sourceID); err != nil {
			return r, fmt.Errorf("resolve restriction label source: %w", err)
		}
	}
	if sourceID > 0 {
		lock, err := s.acquireSyncExecutionLock(ctx, sourceID)
		if err != nil {
			return r, fmt.Errorf("lock restriction source: %w", err)
		}
		execution := &SyncExecution{store: s.withoutSyncScope(), sourceID: sourceID, lock: lock}
		defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	}
	var source, label, person any
	switch r.Kind {
	case "source":
		source = target
	case "label":
		label = target
	case string(AttributeObjectPerson):
		person = target
	}
	err := s.db.QueryRowContext(ctx, s.Rebind(`INSERT INTO agent_restrictions(kind,source_id,label_id,person_id,reveal,note) VALUES(?,?,?,?,?,?) RETURNING id,created_at`), r.Kind, source, label, person, r.Reveal, r.Note).Scan(&r.ID, &r.CreatedAt)
	if err != nil {
		return r, fmt.Errorf("save restriction: %w", err)
	}
	return r, nil
}

func (s *Store) ListAgentRestrictions(ctx context.Context) ([]AgentRestriction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,COALESCE(source_id,0),COALESCE(label_id,0),COALESCE(person_id,0),reveal,note,created_at FROM agent_restrictions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list restrictions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []AgentRestriction{}
	for rows.Next() {
		var r AgentRestriction
		if err := rows.Scan(&r.ID, &r.Kind, &r.SourceID, &r.LabelID, &r.PersonID, &r.Reveal, &r.Note, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan restriction: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) RemoveAgentRestriction(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, s.Rebind(`DELETE FROM agent_restrictions WHERE id=?`), id)
	if err != nil {
		return fmt.Errorf("remove restriction: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ResolveRestrictionLabels resolves current names only; future labels do not match automatically.
func (s *Store) ResolveRestrictionLabels(ctx context.Context, name string, sourceID int64) ([]int64, error) {
	if strings.TrimSpace(name) == "" || sourceID < 0 {
		return nil, errors.New("invalid label selector")
	}
	query := `SELECT id FROM labels WHERE LOWER(name)=LOWER(?)`
	args := []any{name}
	if sourceID != 0 {
		query += ` AND source_id=?`
		args = append(args, sourceID)
	}
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, sql.ErrNoRows
	}
	return ids, nil
}

const agentRestrictionTaintKey = "agent_restriction_taint"

func (s *Store) AgentRestrictionTaint(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key=?`), agentRestrictionTaintKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid restriction taint setting %q", value)
	}
}
func (s *Store) SetAgentRestrictionTaint(ctx context.Context, enabled bool) error {
	value := "false"
	if enabled {
		value = "true"
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`), agentRestrictionTaintKey, value)
	return err
}

// CheckAgentRestrictions reports staged rules matching a message, including thread taint.
// It does not enforce them or indicate that any read is protected.
func (s *Store) CheckAgentRestrictions(ctx context.Context, messageID int64) ([]AgentRestriction, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM messages WHERE id=?)`), messageID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}
	taint, err := s.AgentRestrictionTaint(ctx)
	if err != nil {
		return nil, err
	}
	rules, err := s.ListAgentRestrictions(ctx)
	if err != nil {
		return nil, err
	}
	matches := []AgentRestriction{}
	for _, rule := range rules {
		query := `SELECT EXISTS(SELECT 1 FROM messages m WHERE (m.id=? OR (? AND m.conversation_id=(SELECT conversation_id FROM messages WHERE id=?))) AND (`
		switch rule.Kind {
		case "source":
			query += `m.source_id=?`
		case "label":
			query += `EXISTS(SELECT 1 FROM message_labels ml WHERE ml.message_id=m.id AND ml.label_id=?)`
		case string(AttributeObjectPerson):
			query += `(EXISTS(SELECT 1 FROM person_participants pp WHERE pp.person_id=? AND (pp.participant_id=m.sender_id OR EXISTS(SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=pp.participant_id))))`
		}
		query += `))`
		target := rule.SourceID
		if rule.Kind == "label" {
			target = rule.LabelID
		}
		if rule.Kind == string(AttributeObjectPerson) {
			target = rule.PersonID
		}
		var match bool
		if err := s.db.QueryRowContext(ctx, s.Rebind(query), messageID, taint, messageID, target).Scan(&match); err != nil {
			return nil, err
		}
		if match {
			matches = append(matches, rule)
		}
	}
	return matches, nil
}
