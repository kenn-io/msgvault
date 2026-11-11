package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

type personSweepPolicyKey struct{}
type personSweepPolicyWork struct {
	personID int64
	revision string
	inputs   []peoplesweep.EvidenceItem
}

// CapturePersonSweepPolicyWork retains the original person input revision before
// assembly. The existing semantic version includes persons.revision and rendered text.
func (s *Store) CapturePersonSweepPolicyWork(ctx context.Context, personID int64) (context.Context, error) {
	ctx, err := s.CaptureConversationPolicyWork(ctx)
	if err != nil {
		return nil, err
	}
	if old, ok := ctx.Value(personSweepPolicyKey{}).(personSweepPolicyWork); ok {
		if old.personID != personID {
			return nil, ErrConversationPolicyStale
		}
		return ctx, nil
	}
	var revision string
	err = s.withConversationPolicyPublicationLogged(ctx, func(tx *loggedTx) error {
		doc, err := s.loadPersonSemanticDocumentLoggedTx(ctx, tx, personID)
		if err != nil {
			return err
		}
		revision = doc.Revision
		return nil
	})
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, personSweepPolicyKey{}, personSweepPolicyWork{personID: personID, revision: revision}), nil
}

// RefreshPersonSweepPolicyWork rebinds the person input revision after the
// attempt's own status phase changed the person's facts. The worker assembles
// its packet again after that phase. The original conversation policy revision
// and bound inputs stay unchanged.
func (s *Store) RefreshPersonSweepPolicyWork(ctx context.Context, personID int64) (context.Context, error) {
	work, ok := ctx.Value(personSweepPolicyKey{}).(personSweepPolicyWork)
	if !ok || work.personID != personID {
		return nil, ErrConversationPolicyStale
	}
	err := s.withConversationPolicyLocked(ctx, nil, func(tx *loggedTx) error {
		if err := s.CheckConversationPolicyPublicationTx(ctx, tx.Tx, nil); err != nil {
			return err
		}
		doc, err := s.loadPersonSemanticDocumentLoggedTx(ctx, tx, personID)
		if errors.Is(err, ErrPersonNotFound) {
			return ErrConversationPolicyStale
		}
		if err != nil {
			return err
		}
		work.revision = doc.Revision
		return nil
	})
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, personSweepPolicyKey{}, work), nil
}

// BindPersonSweepPolicyInputs keeps the exact assembled source versions through
// retries and commit. Additional brief inputs extend, never replace, that set.
func (s *Store) BindPersonSweepPolicyInputs(ctx context.Context, inputs []peoplesweep.EvidenceItem) (context.Context, error) {
	work, ok := ctx.Value(personSweepPolicyKey{}).(personSweepPolicyWork)
	if !ok {
		return ctx, errors.New("person sweep policy work was not captured")
	}
	work.inputs = append(append([]peoplesweep.EvidenceItem(nil), work.inputs...), inputs...)
	return context.WithValue(ctx, personSweepPolicyKey{}, work), nil
}

// checkPersonSweepPolicyInputsTx reads through the actual publication transaction.
// The snapshot binding reuses existing hydration without opening another pool connection.
func (s *Store) checkPersonSweepPolicyInputsTx(ctx context.Context, tx *loggedTx) error {
	work, ok := ctx.Value(personSweepPolicyKey{}).(personSweepPolicyWork)
	if !ok {
		return nil
	}
	doc, err := s.loadPersonSemanticDocumentLoggedTx(ctx, tx, work.personID)
	if errors.Is(err, ErrPersonNotFound) {
		return ErrConversationPolicyStale
	}
	if err != nil {
		return err
	}
	if doc.Revision != work.revision {
		return ErrConversationPolicyStale
	}
	ctx = context.WithValue(ctx, readSnapshotKey{}, &readSnapshot{db: s.db.DB, tx: tx})
	// Bound work is cumulative across assembly batches and the brief window.
	// Deduplicate hydration, but compare every original citation below.
	grouped := make(map[peoplesweep.SourceClass][]int64)
	for _, original := range work.inputs {
		if original.Tombstone {
			continue
		}
		if original.PersonID != work.personID {
			return ErrConversationPolicyStale
		}
		grouped[original.SourceClass] = append(grouped[original.SourceClass], original.Ref.MessageID)
	}
	hydrated := make(map[peoplesweep.SourceClass]map[int64]peoplesweep.EvidenceItem, len(grouped))
	for class, ids := range grouped {
		ids = uniquePersonSweepMessageIDs(ids)
		current := make(map[int64]peoplesweep.EvidenceItem, len(ids))
		for start := 0; start < len(ids); start += maxPersonSweepEvidenceRows {
			end := min(start+maxPersonSweepEvidenceRows, len(ids))
			items, _, err := s.hydratePersonSweepMessageSet(ctx, work.personID, class, ids[start:end], 0)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConversationPolicyStale
			}
			if err != nil {
				return err
			}
			for _, item := range items {
				current[item.Ref.MessageID] = item
			}
		}
		hydrated[class] = current
	}
	for _, original := range work.inputs {
		if original.Tombstone {
			continue
		}
		if original.PersonID != work.personID {
			return ErrConversationPolicyStale
		}
		current, found := hydrated[original.SourceClass][original.Ref.MessageID]
		if !found {
			return ErrConversationPolicyStale
		}
		if original.Ref.SourceID != current.Ref.SourceID || original.Ref.SourceMessageID != current.Ref.SourceMessageID || original.SourceVersion != current.SourceVersion || original.ContentSHA256 != current.ContentSHA256 || !original.EventTime.Equal(current.EventTime) || !original.RecordedTime.Equal(current.RecordedTime) || !samePersonSweepSubject(original.SubjectPersonID, current.SubjectPersonID) || original.Directness != current.Directness || original.Authority != current.Authority || !reflect.DeepEqual(original.Provenance, current.Provenance) {
			return ErrConversationPolicyStale
		}
	}
	return nil
}

// CheckPersonSweepPolicyWork releases its short transaction before network IO.
func (s *Store) CheckPersonSweepPolicyWork(ctx context.Context) error {
	return CheckConversationPolicyAdmission(ctx)
}

// Direct durable commits retain their cited coordinates even without a worker
// context. They join the same bounded lock set as assembled packet inputs.
type personSweepPublicationRefsKey struct{}

func (s *Store) lockPersonSweepPolicyInputsTx(ctx context.Context, tx *loggedTx) error {
	refs, _ := ctx.Value(personSweepPublicationRefsKey{}).([]peoplesweep.EvidenceRef)
	refs = append([]peoplesweep.EvidenceRef(nil), refs...)
	if work, ok := ctx.Value(personSweepPolicyKey{}).(personSweepPolicyWork); ok {
		for _, input := range work.inputs {
			if !input.Tombstone {
				refs = append(refs, input.Ref)
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	// Recipient replacement takes this lock before message and person rows.
	// Taking it before the cited-message locks keeps one order and avoids a
	// PostgreSQL deadlock with a concurrent replacement.
	if err := s.lockRecipientContextInvalidationTxContext(ctx, tx); err != nil {
		return err
	}
	var messages []int64
	for _, ref := range refs {
		messages = append(messages, ref.MessageID)
	}
	slices.Sort(messages)
	for _, id := range slices.Compact(messages) {
		if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
