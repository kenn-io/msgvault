package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrXTargetChanged        = errors.New("X reply target changed")
	ErrXReplyFrozen          = errors.New("X reply draft is frozen")
	ErrXAttemptState         = errors.New("invalid X reply attempt transition")
	ErrXReplyReceiptNotFound = errors.New("X reply receipt not found")
)

// XAttemptKey identifies the exact durable authority; a draft ID alone never
// authorizes an outcome transition or another provider request.
type XAttemptKey struct {
	SourceID           int64
	DraftID, AttemptID string
}
type XReplyAttempt struct {
	OwnerConfirmed bool
	XAttemptKey
	OwnerID                                                                string
	Generation, Revision                                                   int64
	TargetPostID, TargetCanonicalID                                        string
	TargetMessageID, ConversationID                                        int64
	SourceConversationID, BodyDigest, State, Code, RemoteID, ReservationID string
	ReservedMicros                                                         int64
	CreatedAt                                                              time.Time
}
type XReplyAdmission struct {
	SourceID         int64
	OwnerID, DraftID string
	Revision         int64
	TargetPostID     string
	Cost             XRequestCost
}
type XReplyAdmissionResult struct {
	Attempt XReplyAttempt
	Created bool
}

const xAttemptSelect = `SELECT source_id,draft_id,attempt_id,owner_id,credential_generation,draft_revision,
 target_post_id,COALESCE(target_canonical_id,''),COALESCE(target_message_id,0),COALESCE(conversation_id,0),
 source_conversation_id,body_digest,state,COALESCE(result_code,''),COALESCE(remote_id,''),reservation_id,reserved_micros,created_at,adoption_confirmed_at IS NOT NULL FROM x_reply_attempts `

func scanXAttempt(row scanner) (XReplyAttempt, error) {
	var a XReplyAttempt
	err := row.Scan(&a.SourceID, &a.DraftID, &a.AttemptID, &a.OwnerID, &a.Generation, &a.Revision, &a.TargetPostID, &a.TargetCanonicalID, &a.TargetMessageID, &a.ConversationID, &a.SourceConversationID, &a.BodyDigest, &a.State, &a.Code, &a.RemoteID, &a.ReservationID, &a.ReservedMicros, &a.CreatedAt, &a.OwnerConfirmed)
	return a, err
}
func loadXAttempt(ctx context.Context, q contextRowQuerier, key XAttemptKey) (XReplyAttempt, error) {
	a, err := scanXAttempt(q.QueryRowContext(ctx, xAttemptSelect+`WHERE source_id=? AND draft_id=? AND attempt_id=?`, key.SourceID, key.DraftID, key.AttemptID))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrXAttemptState
	}
	return a, err
}
func xReplyReceipt(ctx context.Context, q contextRowQuerier, draftID string) (XReplyAttempt, error) {
	a, err := scanXAttempt(q.QueryRowContext(ctx, xAttemptSelect+`WHERE draft_id=? ORDER BY CASE WHEN state='rejected' THEN 1 ELSE 0 END,created_at DESC,attempt_id DESC LIMIT 1`, draftID))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrXReplyReceiptNotFound
	}
	return a, err
}
func (s *Store) GetXReplyReceiptContext(ctx context.Context, draftID string) (XReplyAttempt, error) {
	a, err := xReplyReceipt(ctx, s.db, draftID)
	if err != nil {
		return XReplyAttempt{}, err
	}
	if err := s.requireSyncSource(a.SourceID); err != nil {
		return XReplyAttempt{}, err
	}
	return a, nil
}

// lockChatDraftTx is shared by edit/delete/admit/finalize. PostgreSQL must lock
// the draft row itself before checking receipts; a NOT EXISTS predicate does
// not serialize these operations under READ COMMITTED.
func (s *Store) lockChatDraftTx(ctx context.Context, tx *loggedTx, id string) error {
	if query := s.dialect.RowWriterLockSQL("chat_drafts", "updated_at"); query != "" {
		query = strings.Replace(query, "WHERE id = ?", "WHERE draft_id = ?", 1)
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	var found string
	err := tx.QueryRowContext(ctx, `SELECT draft_id FROM chat_drafts WHERE draft_id=?`+s.dialect.SelectForUpdate(), id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChatDraftNotFound
	}
	return err
}
func checkXDraftMutable(ctx context.Context, tx *loggedTx, id string) error {
	var frozen bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM x_reply_attempts WHERE draft_id=? AND state IN ('admitted','remote_unknown','remote_accepted','sent'))`, id).Scan(&frozen)
	if err != nil {
		return err
	}
	if frozen {
		return ErrXReplyFrozen
	}
	return nil
}
func xBodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// AdmitXReplyContext commits both the spend reservation and body-digest-only
// receipt before I/O. Only Created=true confers this invocation's single POST.
func (s *Store) AdmitXReplyContext(ctx context.Context, in XReplyAdmission) (XReplyAdmissionResult, error) {
	if in.SourceID <= 0 || in.OwnerID == "" || in.OwnerID != in.Cost.OwnerID || in.Cost.Kind != XRequestCreate {
		return XReplyAdmissionResult{}, ErrXBudgetIdentity
	}
	if err := s.requireSyncSource(in.SourceID); err != nil {
		return XReplyAdmissionResult{}, err
	}
	var result XReplyAdmissionResult
	err := s.withAttributionTxContext(ctx, attributionLock{Exclusive: true, Sources: []int64{in.SourceID}}, func(tx *loggedTx) error {
		// Receipt precedes draft-not-found. Existing uncertainty is never permission.
		prior, err := xReplyReceipt(ctx, tx, in.DraftID)
		if err == nil && prior.State != "rejected" {
			if prior.SourceID != in.SourceID || prior.OwnerID != in.OwnerID {
				return ErrXBudgetIdentity
			}
			if prior.Revision != in.Revision {
				return ErrChatDraftRevisionConflict
			}
			if prior.TargetPostID != in.TargetPostID {
				return ErrXTargetChanged
			}
			result.Attempt = prior
			return nil
		}
		if err != nil && !errors.Is(err, ErrXReplyReceiptNotFound) {
			return err
		}
		if err := s.lockChatDraftTx(ctx, tx, in.DraftID); err != nil {
			return err
		}
		// A concurrent admission may have committed while we waited on the row.
		prior, err = xReplyReceipt(ctx, tx, in.DraftID)
		if err == nil && prior.State != "rejected" {
			if prior.SourceID != in.SourceID || prior.OwnerID != in.OwnerID {
				return ErrXBudgetIdentity
			}
			if prior.Revision != in.Revision {
				return ErrChatDraftRevisionConflict
			}
			if prior.TargetPostID != in.TargetPostID {
				return ErrXTargetChanged
			}
			result.Attempt = prior
			return nil
		}
		if err != nil && !errors.Is(err, ErrXReplyReceiptNotFound) {
			return err
		}
		d, err := loadChatDraft(ctx, tx, in.DraftID)
		if err != nil {
			return err
		}
		if d.SourceType != "x" || d.SourceID != in.SourceID || d.SourceIdentifier != in.OwnerID {
			return ErrXBudgetIdentity
		}
		if d.Revision != in.Revision || in.Revision <= 0 {
			return ErrChatDraftRevisionConflict
		}
		if d.ReplyToSourceMessageID != in.TargetPostID {
			return ErrXTargetChanged
		}
		target, err := s.resolveXPostTx(ctx, tx, d.SourceID, d.ReplyToSourceMessageID)
		if err != nil || target.MessageID == 0 || target.Availability != "public" {
			return ErrChatDraftInvalidDestination
		}
		if target.CurrentID != d.ReplyToSourceMessageID || target.ConversationID != d.ConversationID {
			return ErrXTargetChanged
		}
		var native string
		if err := tx.QueryRowContext(ctx, `SELECT source_conversation_id FROM conversations WHERE id=? AND source_id=?`, d.ConversationID, d.SourceID).Scan(&native); err != nil {
			return ErrChatDraftInvalidDestination
		}
		if native != d.SourceConversationID {
			return ErrXTargetChanged
		}
		reservation, err := s.reserveXRequestTx(ctx, tx, in.SourceID, in.Cost)
		if err != nil {
			return err
		}
		a := XReplyAttempt{XAttemptKey: XAttemptKey{SourceID: in.SourceID, DraftID: in.DraftID, AttemptID: fmt.Sprintf("xattempt_%020d_%s", time.Now().UnixNano(), rand.Text())}, OwnerID: in.OwnerID, Generation: in.Cost.Generation, Revision: d.Revision, TargetPostID: d.ReplyToSourceMessageID, TargetCanonicalID: target.CanonicalID, TargetMessageID: target.MessageID, ConversationID: d.ConversationID, SourceConversationID: d.SourceConversationID, BodyDigest: xBodyDigest(d.Body), State: "admitted", ReservationID: reservation.ID, ReservedMicros: reservation.AmountMicros, CreatedAt: time.Now().UTC()}
		_, err = tx.ExecContext(ctx, `INSERT INTO x_reply_attempts(source_id,draft_id,attempt_id,owner_id,credential_generation,draft_revision,target_post_id,target_canonical_id,target_message_id,conversation_id,source_conversation_id,body_digest,state,reservation_id,reserved_micros,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.SourceID, a.DraftID, a.AttemptID, a.OwnerID, a.Generation, a.Revision, a.TargetPostID, a.TargetCanonicalID, a.TargetMessageID, a.ConversationID, a.SourceConversationID, a.BodyDigest, a.State, a.ReservationID, a.ReservedMicros, a.CreatedAt)
		if err != nil {
			return err
		}
		result.Attempt, err = loadXAttempt(ctx, tx, a.XAttemptKey)
		result.Created = err == nil
		return err
	})
	if err != nil {
		return XReplyAdmissionResult{}, err
	}
	return result, nil
}

func (s *Store) transitionXReply(ctx context.Context, key XAttemptKey, fn func(*loggedTx, XReplyAttempt) error) error {
	if key.SourceID <= 0 || key.DraftID == "" || key.AttemptID == "" {
		return ErrXAttemptState
	}
	if err := s.requireSyncSource(key.SourceID); err != nil {
		return err
	}
	return s.withAttributionTxContext(ctx, attributionLock{Exclusive: true, Sources: []int64{key.SourceID}}, func(tx *loggedTx) error {
		if err := s.lockChatDraftTx(ctx, tx, key.DraftID); err != nil && !errors.Is(err, ErrChatDraftNotFound) {
			return err
		}
		a, err := loadXAttempt(ctx, tx, key)
		if err != nil {
			return err
		}
		return fn(tx, a)
	})
}
func (s *Store) RecordXReplyAcceptedContext(ctx context.Context, key XAttemptKey, remoteID string) error {
	return s.recordXReplyAccepted(ctx, key, remoteID, false)
}

// RecordXReplyAdoptedContext records the owner's explicit association after the
// service verified the budgeted lookup. It cannot manufacture an attempt or
// replace an accepted ID. The frozen revision/body/native pins are rechecked.
func (s *Store) RecordXReplyAdoptedContext(ctx context.Context, key XAttemptKey, remoteID string) error {
	return s.recordXReplyAccepted(ctx, key, remoteID, true)
}
func (s *Store) recordXReplyAccepted(ctx context.Context, key XAttemptKey, remoteID string, ownerConfirmed bool) error {
	if !validXControlID(remoteID) {
		return ErrXAttemptState
	}
	return s.transitionXReply(ctx, key, func(tx *loggedTx, a XReplyAttempt) error {
		if a.RemoteID != "" {
			if a.RemoteID == remoteID && (a.State == "remote_accepted" || a.State == "sent") {
				return nil
			}
			return ErrXAttemptState
		}
		if a.State != "admitted" && a.State != "remote_unknown" {
			return ErrXAttemptState
		}
		if ownerConfirmed {
			d, err := loadChatDraft(ctx, tx, a.DraftID)
			if err != nil {
				return err
			}
			if !matchesFrozenXReply(d, a) {
				return ErrXReplyFrozen
			}
			_, err = tx.ExecContext(ctx, `UPDATE x_reply_attempts SET state='remote_accepted',remote_id=?,result_code=NULL,accepted_at=`+s.dialect.Now()+`,adoption_confirmed_at=`+s.dialect.Now()+`,adoption_owner_id=?,adopted_post_id=?,updated_at=`+s.dialect.Now()+` WHERE source_id=? AND draft_id=? AND attempt_id=?`, remoteID, a.OwnerID, remoteID, key.SourceID, key.DraftID, key.AttemptID)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE x_reply_attempts SET state='remote_accepted',remote_id=?,result_code=NULL,accepted_at=`+s.dialect.Now()+`,updated_at=`+s.dialect.Now()+` WHERE source_id=? AND draft_id=? AND attempt_id=?`, remoteID, key.SourceID, key.DraftID, key.AttemptID)
		return err
	})
}
func matchesFrozenXReply(d ChatDraft, a XReplyAttempt) bool {
	return d.SourceType == "x" && d.SourceID == a.SourceID && d.SourceIdentifier == a.OwnerID && d.Revision == a.Revision && d.ReplyToSourceMessageID == a.TargetPostID && d.SourceConversationID == a.SourceConversationID && xBodyDigest(d.Body) == a.BodyDigest
}
func (s *Store) RejectXReplyContext(ctx context.Context, key XAttemptKey, code string) error {
	switch code {
	case "provider_rejected", "credits_exhausted", "rate_limited", "text_rejected", "unauthorized":
	default:
		return ErrXAttemptState
	}
	return s.transitionXReply(ctx, key, func(tx *loggedTx, a XReplyAttempt) error {
		if a.State == "rejected" && a.Code == code {
			return nil
		}
		if a.State != "admitted" {
			return ErrXAttemptState
		}
		_, err := tx.ExecContext(ctx, `UPDATE x_reply_attempts SET state='rejected',result_code=?,updated_at=`+s.dialect.Now()+` WHERE source_id=? AND draft_id=? AND attempt_id=?`, code, key.SourceID, key.DraftID, key.AttemptID)
		return err
	})
}
func (s *Store) MarkXReplyUnknownContext(ctx context.Context, key XAttemptKey) error {
	return s.transitionXReply(ctx, key, func(tx *loggedTx, a XReplyAttempt) error {
		if a.State == "remote_unknown" {
			return nil
		}
		if a.State != "admitted" {
			return ErrXAttemptState
		}
		_, err := tx.ExecContext(ctx, `UPDATE x_reply_attempts SET state='remote_unknown',result_code='remote_unknown',updated_at=`+s.dialect.Now()+` WHERE source_id=? AND draft_id=? AND attempt_id=?`, key.SourceID, key.DraftID, key.AttemptID)
		return err
	})
}

// FinishXReplyContext atomically publishes an accepted own reply, settles its
// original reservation and removes exactly the frozen revision. An existing
// provider projection is retained verbatim; suppression always wins.
func (s *Store) FinishXReplyContext(ctx context.Context, key XAttemptKey) (XReplyAttempt, error) {
	var result XReplyAttempt
	err := s.transitionXReply(ctx, key, func(tx *loggedTx, a XReplyAttempt) error {
		if a.State == "sent" {
			result = a
			return nil
		}
		if a.State != "remote_accepted" || !validXControlID(a.RemoteID) {
			return ErrXAttemptState
		}
		d, err := loadChatDraft(ctx, tx, a.DraftID)
		if err != nil {
			return err
		}
		if !matchesFrozenXReply(d, a) {
			return ErrXReplyFrozen
		}
		suppressed, _, err := s.xGlobalSuppressionTx(ctx, tx, []string{a.RemoteID})
		if err != nil {
			return err
		}
		alias, aliasErr := s.xAliasTx(ctx, tx, a.SourceID, a.RemoteID)
		if aliasErr != nil && !errors.Is(aliasErr, ErrXPostNotFound) {
			return aliasErr
		}
		if !suppressed.Valid && aliasErr == nil && alias.MessageID.Valid {
			prior, err := s.loadXStoredPostTx(ctx, tx, a.SourceID, alias.MessageID.Int64)
			if err != nil {
				return err
			}
			if !prior.MetadataKnown || prior.Metadata.AuthorID != a.OwnerID || prior.Metadata.ReferencedMessageID != a.TargetPostID || prior.Metadata.OriginalConversationID != a.SourceConversationID {
				return ErrXAttemptState
			}
		}
		if suppressed.Valid || aliasErr != nil || !alias.MessageID.Valid {
			// This is submitted text, not a fetched provider snapshot. Unknown profile
			// availability cannot publish a handle or clear protected evidence.
			// The writer also retains an ID-only local alias for foreign suppression,
			// so removing that other source cannot reopen this accepted post.
			_, err = s.persistXPostTx(ctx, tx, XPersistPostInput{SourceID: a.SourceID, OwnerID: a.OwnerID, PostID: a.RemoteID, AuthorID: a.OwnerID, ConversationID: a.SourceConversationID, ReplyToPostID: a.TargetPostID, EditHistory: []string{a.RemoteID}, Text: d.Body, TextIncomplete: true, CreatedAt: a.CreatedAt, FetchedAt: a.CreatedAt, Availability: "unverified", EstimatedTimestamp: true})
			if err != nil {
				return err
			}
		}
		if err := s.settleXRequestTx(ctx, tx, a.SourceID, a.ReservationID, XResourceUsage{Persisted: true, Complete: true, Created: true}); err != nil {
			return err
		}
		deleted, err := tx.ExecContext(ctx, `DELETE FROM chat_drafts WHERE draft_id=? AND source_id=? AND revision=?`, a.DraftID, a.SourceID, a.Revision)
		if err := chatDraftWriteResult(ctx, tx, a.DraftID, deleted, err); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE x_reply_attempts SET state='sent',result_code=NULL,updated_at=`+s.dialect.Now()+` WHERE source_id=? AND draft_id=? AND attempt_id=?`, key.SourceID, key.DraftID, key.AttemptID)
		if err != nil {
			return err
		}
		result, err = loadXAttempt(ctx, tx, key)
		return err
	})
	if err != nil {
		return XReplyAttempt{}, err
	}
	return result, nil
}
