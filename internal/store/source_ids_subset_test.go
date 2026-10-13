package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSourceLifetimeSubsetInitializesCopiedMaximum(t *testing.T) {
	for _, profiles := range []bool{false, true} {
		t.Run(strconv.FormatBool(profiles), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			srcPath := filepath.Join(t.TempDir(), "source.db")
			src, err := store.Open(srcPath)
			requirements.NoError(err)
			requirements.NoError(src.InitSchema())
			main, err := src.GetOrCreateSource("gmail", "subset@example.test")
			requirements.NoError(err)
			sender, err := src.EnsureParticipant("sender@example.test", "Synthetic Sender", "example.test")
			requirements.NoError(err)
			recipient, err := src.EnsureParticipant("recipient@example.test", "Synthetic Recipient", "example.test")
			requirements.NoError(err)
			_, err = src.DB().Exec(`INSERT INTO conversations(id,source_id,conversation_type) VALUES(1,?,'email_thread')`, main.ID)
			requirements.NoError(err)
			_, err = src.DB().Exec(`INSERT INTO messages(id,conversation_id,source_id,source_message_id,message_type,sent_at,sender_id) VALUES(1,1,?,'subset-message','email','2026-01-01 00:00:00',?)`, main.ID, sender)
			requirements.NoError(err)
			_, err = src.DB().Exec(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(1,?,'to')`, recipient)
			requirements.NoError(err)
			expectedHighest := main.ID
			if profiles {
				observationSource, err := src.GetOrCreateSource("contacts", "profile-only@example.test")
				requirements.NoError(err)
				_, err = src.RecordContactObservationContext(t.Context(), sender, store.ParticipantContactObservationInput{SourceID: &observationSource.ID, AddressKind: store.ContactAddressEmail, OriginalValue: "profile@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}})
				requirements.NoError(err)
				supportSource, err := src.GetOrCreateSource("contacts", "support-only@example.test")
				requirements.NoError(err)
				normalized := "synthetic-subset-provider"
				_, _, err = src.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{LeftKind: store.IdentityMatchParticipant, LeftID: sender, RightKind: store.IdentityMatchParticipant, RightID: recipient, Basis: store.IdentityMatchStableProviderID, NormalizedValue: &normalized, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &supportSource.ID})
				requirements.NoError(err)
				expectedHighest = supportSource.ID
			}
			requirements.NoError(src.Close())
			dst := filepath.Join(t.TempDir(), "subset")
			_, err = store.CopySubsetWithOptions(srcPath, dst, 1, store.CopySubsetOptions{IncludeProfiles: profiles})
			requirements.NoError(err)
			st, err := store.Open(filepath.Join(dst, "msgvault.db"))
			requirements.NoError(err)
			defer func() { requirements.NoError(st.Close()) }()
			var highest int64
			requirements.NoError(st.DB().QueryRow(`SELECT MAX(id) FROM sources`).Scan(&highest))
			assertions.Equal(expectedHighest, highest, "include both dynamic profile and support source-copy paths")
			var retained string
			requirements.NoError(st.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='source_id_high_water'`).Scan(&retained), "counter must be initialized in the copy transaction before allocation/removal")
			assertions.Equal(strconv.FormatInt(highest, 10), retained)
			requirements.NoError(st.RemoveSource(highest))
			next, err := st.GetOrCreateSource("gmail", "subset-next@example.test")
			requirements.NoError(err)
			assertions.Greater(next.ID, highest)
		})
	}
}
