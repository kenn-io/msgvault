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
		if original.SourceClass != peoplesweep.SourceDocumentText {
			grouped[original.SourceClass] = append(grouped[original.SourceClass], original.Ref.MessageID)
		}
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
		var current peoplesweep.EvidenceItem
		var err error
		if original.SourceClass == peoplesweep.SourceDocumentText {
			current, err = s.alignDocumentItem(ctx, work.personID, original.Ref, original.Excerpt)
		} else {
			var found bool
			current, found = hydrated[original.SourceClass][original.Ref.MessageID]
			if !found {
				return ErrConversationPolicyStale
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConversationPolicyStale
		}
		if err != nil {
			return err
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
	var messages, attachments []int64
	for _, ref := range refs {
		messages = append(messages, ref.MessageID)
		if ref.SourceLane == peoplesweep.SourceDocumentText {
			attachments = append(attachments, ref.AttachmentID)
		}
	}
	slices.Sort(messages)
	for _, id := range slices.Compact(messages) {
		if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
			return err
		}
	}
	slices.Sort(attachments)
	var hashes []string
	for _, id := range slices.Compact(attachments) {
		if err := s.lockDocumentOccurrenceAttachmentTx(ctx, tx, id); err != nil {
			return err
		}
		query := `SELECT COALESCE(content_hash,''), COALESCE(storage_path,'') FROM attachments WHERE id=?`
		if s.IsPostgreSQL() {
			query += " FOR SHARE"
		}
		var file FileMetadata
		if err := tx.QueryRowContext(ctx, query, id).Scan(&file.ContentHash, &file.StoragePath); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConversationPolicyStale
			}
			return err
		}
		normalizeFileMetadataStorage(&file, "")
		hashes = append(hashes, file.ContentHash)
	}
	slices.Sort(hashes)
	for _, hash := range slices.Compact(hashes) {
		if err := s.lockDocumentPublicationHashTx(ctx, tx, hash); err != nil {
			return err
		}
	}
	if len(attachments) > 0 && s.IsPostgreSQL() {
		// Extraction publication takes hash before revision. Profile retirement,
		// target changes and terminal outcomes also update this existing row before
		// commit; holding it keeps their visibility stable through our validation.
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM document_index_state WHERE singleton=1 FOR SHARE`).Scan(&revision); err != nil {
			return err
		}
	}
	return nil
}
