package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"hash"
	"slices"
	"strings"
)

const xRestoreFinancialDomain = "msgvault-x-restore-financial-v1\n"

// Only authority is fingerprinted. Content, credentials, pagination and
// retention progress cannot be reconstructed from this restore proof.
type xRestoreFinancialState struct {
	ModelError   string              `json:"model_error,omitempty"`
	Spend        []XSpendBucket      `json:"spend,omitempty"`
	Reservations []XReservation      `json:"reservations,omitempty"`
	Jobs         []xRestoreJobBudget `json:"jobs,omitempty"`
}

type xRestoreJobBudget struct {
	ID, Fingerprint      string
	Cap, Spent, Reserved int64
}

// The domain and empty JSON object are identical to the no-activity projection
// below. A current source with this head may be absent from an older snapshot;
// its global removal IDs can still be applied through a restored source.
func emptyXRestoreFinancialHead() string {
	digest := sha256.Sum256([]byte(xRestoreFinancialDomain + "{}\n"))
	return hex.EncodeToString(digest[:])
}

func writeXRestoreFingerprint(h hash.Hash, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, _ = h.Write(data)
	_, _ = h.Write([]byte{'\n'})
	return nil
}

func (s *Store) xRestoreFinancialHeadTx(ctx context.Context, tx *loggedTx, sourceID int64, state XControlState) (string, error) {
	projection := xRestoreFinancialState{
		ModelError: state.ModelError, Spend: slices.Clone(state.SpendBuckets),
		Reservations: slices.Clone(state.Reservations),
	}
	slices.SortFunc(projection.Spend, func(a, b XSpendBucket) int { return strings.Compare(a.Month, b.Month) })
	slices.SortFunc(projection.Reservations, func(a, b XReservation) int { return strings.Compare(a.ID, b.ID) })
	for _, job := range state.Jobs {
		projection.Jobs = append(projection.Jobs, xRestoreJobBudget{
			ID: job.ID, Fingerprint: job.Fingerprint, Cap: job.CapMicros,
			Spent: job.SpentMicros, Reserved: job.ReservedMicros,
		})
	}
	slices.SortFunc(projection.Jobs, func(a, b xRestoreJobBudget) int { return strings.Compare(a.ID, b.ID) })
	h := sha256.New()
	_, _ = h.Write([]byte(xRestoreFinancialDomain))
	if err := writeXRestoreFingerprint(h, projection); err != nil {
		return "", err
	}
	// Stream existing content-free receipts rather than exporting their rows or
	// allowing an unbounded in-memory send journal. Local FK repair/timestamps
	// do not alter authority; native pins, identities and state do.
	rows, err := tx.QueryContext(ctx, `SELECT draft_id,attempt_id,owner_id,
		credential_generation,draft_revision,target_post_id,
		COALESCE(target_canonical_id,''),source_conversation_id,body_digest,state,
		COALESCE(result_code,''),COALESCE(remote_id,''),reservation_id,reserved_micros,
		COALESCE(adoption_owner_id,''),COALESCE(adopted_post_id,'')
		FROM x_reply_attempts WHERE source_id=? ORDER BY draft_id,attempt_id`, sourceID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var attempt struct {
			Draft, Attempt, Owner                              string
			Generation, Revision                               int64
			Target, Canonical, Conversation, BodyDigest, State string
			Code, RemoteID, Reservation                        string
			Reserved                                           int64
			AdoptionOwner, AdoptedPost                         string
		}
		if err := rows.Scan(&attempt.Draft, &attempt.Attempt, &attempt.Owner,
			&attempt.Generation, &attempt.Revision, &attempt.Target, &attempt.Canonical,
			&attempt.Conversation, &attempt.BodyDigest, &attempt.State, &attempt.Code,
			&attempt.RemoteID, &attempt.Reservation, &attempt.Reserved,
			&attempt.AdoptionOwner, &attempt.AdoptedPost); err != nil {
			_ = rows.Close()
			return "", err
		}
		if err := writeXRestoreFingerprint(h, attempt); err != nil {
			_ = rows.Close()
			return "", err
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
