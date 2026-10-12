package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

func TestConflictPublicationCannotInheritStandardPendingIntentIdentity(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, book, mapping := seededCardDAVConflictMapping(t)
	original := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Standard")
	requirements.NotEmpty(original.PendingIntentID)
	current, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	requirements.NoError(err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), original.PersonID)
	requirements.NoError(err)
	capture := store.CardDAVConflictCapture{AddressBookID: book.ID, Href: mapping.Href, ExpectedMappingRevision: current.MappingRevision, BaseLocalHash: current.LocalHash, LocalHash: snapshot.Fingerprint, BaseRemoteHash: current.RemoteSemanticHash, BaseRemoteETag: current.RemoteETag, RemoteETag: `"synthetic-conflicting"`, LocalBody: original.OutgoingBody, RemoteBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-conflicting\r\nFN:Synthetic Remote\r\nEND:VCARD\r\n")}
	conflict, err := st.RecordCardDAVPublicationConflictContext(t.Context(), *original, capture)
	requirements.NoError(err)
	cleared, err := st.GetCardDAVPublicationContext(t.Context(), original.PersonID)
	requirements.NoError(err)
	requirements.Empty(cleared.PendingOperation)
	requirements.Empty(cleared.PendingIntentID, "capturing a terminal conflict must retire the standard publication identity")
	hash, err := carddav.SemanticHash(capture.LocalBody)
	requirements.NoError(err)
	prepared, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: conflict.ID, ExpectedPersonID: original.PersonID, ExpectedMappingRevision: conflict.MappingRevision, RemoteETag: capture.RemoteETag, OutgoingSemanticHash: hash})
	requirements.NoError(err)
	requirements.Empty(prepared.PendingIntentID, "new conflict-origin publication must not inherit an unrelated standard intent nonce")
	requirements.Equal(conflict.ID, prepared.ResolutionConflictID)
	backend, ok := any(st).(authorizedPublicationRefreshStore)
	requirements.True(ok)
	seen := false
	_, err = backend.RefreshCardDAVPublicationFenceAuthorizedContext(t.Context(), *prepared, func(_ context.Context, _ *store.IdentityGrantSelection) error { seen = true; return nil })
	requirements.ErrorIs(err, store.ErrCardDAVInvalidPlan)
	assertions.False(seen)
	canonical := store.CardDAVRemoteResource{Href: prepared.Href, RemoteUID: "synthetic-conflicting", RemoteETag: `"synthetic-settled"`, RemoteBody: prepared.OutgoingBody, SemanticHash: prepared.OutgoingSemanticHash}
	requirements.NoError(st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *prepared, Remote: canonical}))
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	requirements.NoError(err)
	assertions.Equal(store.CardDAVConflictResolved, resolved.Status, "owner still settles the native conflict mutation")
}
