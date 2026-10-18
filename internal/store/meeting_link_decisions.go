package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

func backfillMeetingLinkDecisionTargets(ctx context.Context, tx *loggedTx) error {
	rows, err := tx.QueryContext(ctx, `SELECT member_message_id,target_ref FROM meeting_link_decisions WHERE target_message_id IS NULL ORDER BY member_message_id,target_ref`)
	if err != nil {
		return fmt.Errorf("read legacy meeting link decision targets: %w", err)
	}
	type decisionTarget struct {
		member int64
		ref    string
	}
	var targets []decisionTarget
	for rows.Next() {
		var target decisionTarget
		if err := rows.Scan(&target.member, &target.ref); err != nil {
			_ = rows.Close()
			return err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, target := range targets {
		kind, occurrence, _, err := ParseMeetingRef(target.ref)
		if err != nil || kind != calendarEventRefKind {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE meeting_link_decisions SET target_message_id = ? WHERE member_message_id = ? AND target_ref = ? AND target_message_id IS NULL AND EXISTS (SELECT 1 FROM messages WHERE id = ? AND message_type = 'calendar_event')`, occurrence.MessageID, target.member, target.ref, occurrence.MessageID); err != nil {
			return fmt.Errorf("backfill meeting link decision target: %w", err)
		}
	}
	return nil
}

// Consolidation preserves authoritative decisions for both member and target.
// UTC timestamps are written by the decision API; the later decision wins a
// collision rather than being lost through the member's hard-delete cascade.
func (s *Store) repointMeetingLinkDecisionsWith(ctx context.Context, tx *loggedTx, legacyID, survivorID int64) error {
	if err := s.lockMeetingEvidenceWith(ctx, tx, legacyID); err != nil {
		return fmt.Errorf("lock consolidated meeting member: %w", err)
	}
	oldRef := fmt.Sprintf("evt:%d", legacyID)
	rows, err := tx.QueryContext(ctx, `SELECT member_message_id,target_message_id,target_ref,target_logical,decision,created_at FROM meeting_link_decisions WHERE member_message_id = ? OR target_message_id = ? OR target_ref = ? OR target_ref LIKE ? ORDER BY member_message_id,target_ref`, legacyID, legacyID, oldRef, oldRef+"@%")
	if err != nil {
		return fmt.Errorf("read consolidated meeting decisions: %w", err)
	}
	type decision struct {
		member             int64
		target             sql.NullInt64
		ref, logical, kind string
		created            time.Time
	}
	var decisions []decision
	for rows.Next() {
		var d decision
		if err := rows.Scan(&d.member, &d.target, &d.ref, &d.logical, &d.kind, &d.created); err != nil {
			_ = rows.Close()
			return err
		}
		decisions = append(decisions, d)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	q := boundQuerier{ctx: ctx, q: tx}
	affectedMembers := make(map[int64]bool)
	for _, d := range decisions {
		member, ref := d.member, d.ref
		if member == legacyID {
			member = survivorID
		}
		affectedMembers[member] = true
		kind, target, _, err := ParseMeetingRef(ref)
		targetID := d.target
		if err == nil && kind == calendarEventRefKind {
			if target.MessageID == legacyID || (targetID.Valid && targetID.Int64 == legacyID) {
				target.MessageID = survivorID
				ref = FormatOccurrenceRef(target)
				targetID = sql.NullInt64{Int64: survivorID, Valid: true}
			}
		}
		if _, err := q.Exec(`DELETE FROM meeting_link_decisions WHERE member_message_id = ? AND target_ref = ?`, d.member, d.ref); err != nil {
			return fmt.Errorf("remove consolidated decision: %w", err)
		}
		_, err = q.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(member_message_id,target_ref) DO UPDATE SET target_message_id = excluded.target_message_id,target_logical = excluded.target_logical,decision = excluded.decision,created_at = excluded.created_at WHERE meeting_link_decisions.created_at < excluded.created_at`, member, targetID, ref, d.logical, d.kind, d.created.UTC())
		if err != nil {
			return fmt.Errorf("repoint meeting decision: %w", err)
		}
	}
	// Consolidation can combine decisions recorded on different transcript
	// rows. Keep a newer logical split authoritative even when its target ref
	// differs from the older positive decision.
	memberIDs := make([]int64, 0, len(affectedMembers))
	for memberID := range affectedMembers {
		memberIDs = append(memberIDs, memberID)
	}
	slices.Sort(memberIDs)
	for _, memberID := range memberIDs {
		_, err = q.Exec(`DELETE FROM meeting_link_decisions AS link
			WHERE link.member_message_id = ? AND link.decision = 'link' AND link.target_logical <> ''
			  AND EXISTS (
				SELECT 1 FROM meeting_link_decisions AS split
				WHERE split.member_message_id = link.member_message_id
				  AND split.target_logical = link.target_logical AND split.decision = 'split'
				  AND split.created_at > link.created_at
			  )`, memberID)
		if err != nil {
			return fmt.Errorf("remove consolidated links superseded by splits: %w", err)
		}
	}
	return nil
}

// LinkMeetingContext records an explicit owner decision for a visible occurrence.
func (s *Store) LinkMeetingContext(ctx context.Context, ref string, memberID int64) error {
	return s.writeMeetingLinkDecision(ctx, ref, memberID, "link")
}

// UnlinkMeetingContext vetoes this occurrence and UID-equivalent copies.
func (s *Store) UnlinkMeetingContext(ctx context.Context, ref string, memberID int64) error {
	return s.writeMeetingLinkDecision(ctx, ref, memberID, "split")
}

// lockMeetingDecisionEvidenceMessageWith uses a non-key-changing PostgreSQL
// lock. It still serializes message writes but is compatible with the foreign
// key's KEY SHARE lock when consolidation repoints a decision.
func (s *Store) lockMeetingDecisionEvidenceMessageWith(ctx context.Context, tx *loggedTx, messageID int64) error {
	if s.dialect.DriverName() != postgresDriverName {
		return s.lockMeetingEvidenceWith(ctx, tx, messageID)
	}
	q := boundQuerier{ctx: ctx, q: tx}
	var id int64
	err := q.QueryRow(`SELECT id FROM messages WHERE id = ? FOR NO KEY UPDATE`, messageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// lockMeetingDecisionEvidenceWith locks the member and target in ID order so a
// decision serializes with consolidation of either message without reciprocal
// decisions taking locks in opposite order.
func (s *Store) lockMeetingDecisionEvidenceWith(ctx context.Context, tx *loggedTx, memberID, targetID int64) error {
	first, second := memberID, targetID
	if first > second {
		first, second = second, first
	}
	if err := s.lockMeetingDecisionEvidenceMessageWith(ctx, tx, first); err != nil {
		return fmt.Errorf("lock meeting decision message %d: %w", first, err)
	}
	if second == first {
		return nil
	}
	if err := s.lockMeetingDecisionEvidenceMessageWith(ctx, tx, second); err != nil {
		return fmt.Errorf("lock meeting decision message %d: %w", second, err)
	}
	return nil
}

func (s *Store) writeMeetingLinkDecision(ctx context.Context, ref string, memberID int64, decision string) error {
	kind, occ, _, err := ParseMeetingRef(ref)
	if err != nil {
		return err
	}
	if kind != calendarEventRefKind {
		return errors.New("meeting decision requires an event reference")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin meeting decision: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockMeetingDecisionEvidenceWith(ctx, tx, memberID, occ.MessageID); err != nil {
		return fmt.Errorf("lock meeting decision evidence: %w", err)
	}
	r := reboundReader{reader: tx.Tx, rebind: s.dialect.Rebind}
	visible, err := linkMemberVisible(ctx, r, OwnerScope(), memberID)
	if err != nil {
		return err
	}
	if !visible {
		return sql.ErrNoRows
	}
	budget := calrecur.RequestReadBudget
	target, err := resolveLinkOccurrence(ctx, r, OwnerScope(), occ, &budget)
	if err != nil {
		return fmt.Errorf("resolve meeting decision target: %w", err)
	}
	// Different refs can resolve to one occurrence after an orphan's master
	// arrives. Replace those decisions together, retaining distinct calendar copies.
	if err := replaceEquivalentMeetingDecisions(ctx, tx, r, memberID, target); err != nil {
		return err
	}
	logical := ""
	if target.Row.Event.ICalUID != "" {
		logical = occurrenceLogical(target)
	}
	if decision == "split" && logical != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE member_message_id = ? AND decision = 'link' AND target_logical = ?`, memberID, logical); err != nil {
			return fmt.Errorf("supersede equivalent meeting links: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meeting_link_decisions(member_message_id,target_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(member_message_id,target_ref) DO UPDATE SET target_message_id = excluded.target_message_id,target_logical = excluded.target_logical,decision = excluded.decision,created_at = excluded.created_at`, memberID, occ.MessageID, FormatOccurrenceRef(occ), logical, decision, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("write meeting decision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit meeting decision: %w", err)
	}
	return nil
}

func replaceEquivalentMeetingDecisions(ctx context.Context, tx *loggedTx, r rowReader, memberID int64, target linkOccurrence) error {
	rows, err := r.QueryContext(ctx, `SELECT target_ref FROM meeting_link_decisions WHERE member_message_id = ?`, memberID)
	if err != nil {
		return fmt.Errorf("read equivalent meeting decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			_ = rows.Close()
			return err
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	budget := calrecur.RequestReadBudget
	targetLogical := occurrenceLogical(target)
	for _, ref := range refs {
		kind, occ, _, err := ParseMeetingRef(ref)
		if err != nil || kind != calendarEventRefKind {
			continue
		}
		resolved, err := resolveLinkOccurrence(ctx, r, OwnerScope(), occ, &budget)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			continue
		}
		if err != nil {
			return fmt.Errorf("resolve equivalent meeting decision: %w", err)
		}
		if resolved.Ref == target.Ref {
			if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_decisions WHERE member_message_id = ? AND target_ref = ?`, memberID, ref); err != nil {
				return fmt.Errorf("replace equivalent meeting decision: %w", err)
			}
			continue
		}
		if targetLogical != "" && occurrenceLogical(resolved) == targetLogical {
			if _, err := tx.ExecContext(ctx, `UPDATE meeting_link_decisions SET target_logical = ? WHERE member_message_id = ? AND target_ref = ? AND target_logical <> ?`, targetLogical, memberID, ref, targetLogical); err != nil {
				return fmt.Errorf("refresh equivalent meeting decision key: %w", err)
			}
		}
	}
	return nil
}
