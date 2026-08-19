package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vcard"
)

// TestVCardResourceNoOpWriteRejectsReplacementCommittedAfterRead forces the
// interleaving the no-op fast path used to be blind to. A writer whose envelope
// matches what it read has nothing to update, but "nothing changed" was only
// true as of its read snapshot: with no write statement in the transaction,
// SQLite let it commit over a replacement that landed after that snapshot and
// report success at a revision that was no longer live. The revision claim on
// the no-op path is what turns that into the write conflict the caller's
// expected revision promised.
//
// The interleaving is forced through the write body's transaction seam rather
// than raced: the wrapping transaction pins its read snapshot, a second handle
// then replaces the resource, and only after that does the production write
// body run inside the pinned transaction.
func TestVCardResourceNoOpWriteRejectsReplacementCommittedAfterRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "noop-cas.db")
	first, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = first.Close() })
	require.NoError(first.InitSchema())
	participantID, err := first.EnsureParticipant(
		"alice@example.com", "Test User", "example.com",
	)
	require.NoError(err)
	person, _, err := first.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	second, err := OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = second.Close() })

	initial, err := vcard.ParseResourceEnvelope(
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
	)
	require.NoError(err)
	initial.SourceRef, initial.SourceResourceUID = "book", "noop-cas"
	created, err := first.PutVCardResourceEnvelopeContext(ctx, VCardResourceEnvelopeInput{
		PersonID: person.ID, ProjectionFingerprint: "same", Envelope: initial,
	})
	require.NoError(err)

	replacement, err := vcard.ParseResourceEnvelope(
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Replaced\r\nEND:VCARD\r\n"),
	)
	require.NoError(err)
	replacement.SourceRef, replacement.SourceResourceUID = "book", "noop-cas"
	replaced := false
	pinnedThenReplaced := func(ctx context.Context, fn func(tx *loggedTx) error) error {
		return first.withTxContext(ctx, func(tx *loggedTx) error {
			var count int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM vcard_resource_envelopes`,
			).Scan(&count); err != nil {
				return err
			}
			if !replaced {
				replaced = true
				expected := created.Revision
				if _, err := second.PutVCardResourceEnvelopeContext(ctx,
					VCardResourceEnvelopeInput{
						PersonID: person.ID, ExpectedRevision: &expected,
						ProjectionFingerprint: "replaced", Envelope: replacement,
					}); err != nil {
					return err
				}
			}
			return fn(tx)
		})
	}

	// Identical to what the pinned snapshot still shows, so the write body
	// takes the no-op path; the same composition putVCardResourceEnvelopeContext
	// uses, with only the transaction runner swapped.
	input := VCardResourceEnvelopeInput{
		PersonID: person.ID, ExpectedRevision: &created.Revision,
		ProjectionFingerprint: "same", Envelope: created.ResourceEnvelope,
	}
	envelope, err := prepareVCardEnvelope(input.Envelope)
	require.NoError(err)
	_, err = retryContendedWrite(ctx, first, "write vCard resource envelope",
		func() (*VCardResourceEnvelopeRecord, error) {
			return first.writeVCardResourceEnvelopeOnce(
				ctx, pinnedThenReplaced, input, envelope, "",
			)
		})
	require.ErrorIs(err, ErrVCardResourceWriteConflict,
		"a no-op write must not report success at a revision another writer replaced")
	var conflict *VCardResourceWriteConflictError
	require.ErrorAs(err, &conflict)
	assert.Equal(created.Revision, conflict.ExpectedRevision)

	loaded, err := first.GetVCardResourceEnvelopeContext(ctx, "book", "noop-cas")
	require.NoError(err)
	assert.Equal(created.Revision+1, loaded.Revision)
	assert.Contains(string(loaded.StoredBody), "FN:Replaced")
	assert.Equal("replaced", loaded.ProjectionFingerprint)
}
