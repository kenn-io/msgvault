package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestStore_GetSourcesByIdentifier(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Create two sources with same identifier, different types
	_, err := st.GetOrCreateSource("gmail", "user@example.com")
	require.NoError(err, "create gmail source")
	_, err = st.GetOrCreateSource("mbox", "user@example.com")
	require.NoError(err, "create mbox source")

	sources, err := st.GetSourcesByIdentifier("user@example.com")
	require.NoError(err, "GetSourcesByIdentifier")
	require.Len(sources, 2)

	// Verify ordering by source_type
	assert.Equal("gmail", sources[0].SourceType, "sources[0].SourceType")
	assert.Equal("mbox", sources[1].SourceType, "sources[1].SourceType")
}

func TestStore_GetSourcesByIdentifier_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	sources, err := st.GetSourcesByIdentifier("nobody@example.com")
	require.NoError(t, err, "GetSourcesByIdentifier")
	assert.Empty(t, sources)
}

func TestStore_GetSourcesByIdentifierOrDisplayNameIgnoresCase(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "Archive@Example.com")
	require.NoError(err)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE sources SET display_name = ? WHERE id = ?`),
		"Primary Archive", source.ID,
	)
	require.NoError(err)

	byIdentifier, err := st.GetSourcesByIdentifierOrDisplayName("archive@example.com")
	require.NoError(err)
	require.Len(byIdentifier, 1)
	assert.Equal(source.ID, byIdentifier[0].ID)

	byDisplayName, err := st.GetSourcesByIdentifierOrDisplayName("primary archive")
	require.NoError(err)
	require.Len(byDisplayName, 1)
	assert.Equal(source.ID, byDisplayName[0].ID)
}

func TestStore_RemoveSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Create messages, labels, and FTS data
	msgID := f.CreateMessage("msg-remove-1")
	f.CreateMessage("msg-remove-2")

	labels := f.EnsureLabels(map[string]string{
		"INBOX": "Inbox",
	}, "system")
	err := f.Store.ReplaceMessageLabels(msgID, []int64{labels["INBOX"]})
	require.NoError(err, "ReplaceMessageLabels")

	if f.Store.FTS5Available() {
		err = f.Store.UpsertFTS(msgID, "Test", "body", "a@b.com", "", "")
		require.NoError(err, "UpsertFTS")
	}

	// Remove source
	err = f.Store.RemoveSource(f.Source.ID)
	require.NoError(err, "RemoveSource")

	// Verify source is gone
	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "source should be nil after removal")

	// Verify messages are gone
	count, err := f.Store.CountMessagesForSource(f.Source.ID)
	require.NoError(err, "CountMessagesForSource")
	assert.Equal(int64(0), count, "message count")

	// Verify labels are gone
	var labelCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM labels WHERE source_id = ?`), f.Source.ID,
	).Scan(&labelCount)
	require.NoError(err, "count labels")
	assert.Equal(0, labelCount, "label count")

	// Verify FTS rows are gone (SQLite FTS5 vtable only; on PG the
	// equivalent invariant — search_fts cleared — is covered by the
	// dialect-level FTSDeleteSQL test).
	if f.Store.FTS5Available() && !f.Store.IsPostgreSQL() {
		var ftsCount int
		err = f.Store.DB().QueryRow(
			`SELECT COUNT(*) FROM messages_fts`,
		).Scan(&ftsCount)
		require.NoError(err, "count FTS")
		assert.Equal(0, ftsCount, "FTS count")
	}
}

func TestStore_RemoveSource_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	err := st.RemoveSource(99999)
	require.Error(t, err, "RemoveSource should error for nonexistent ID")
}

func recordStableProviderObservationPair(
	ctx context.Context,
	t *testing.T,
	st *store.Store,
	sourceID, left, right int64,
	providerUserID string,
) {
	t.Helper()
	for index, participantID := range []int64{left, right} {
		_, err := st.RecordContactObservationContext(
			ctx, participantID, store.ParticipantContactObservationInput{
				SourceID:       &sourceID,
				AddressKind:    store.ContactAddressEmail,
				ProviderUserID: &providerUserID,
				OriginalValue:  fmt.Sprintf("support-%d-%d@example.org", sourceID, index),
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceArchiveObservation,
				},
			},
		)
		require.NoError(t, err)
	}
}

func TestStore_RemoveSourceRemovesObservationIdentityCandidates(t *testing.T) {
	for _, test := range []struct {
		name   string
		remove func(context.Context, *store.Store, int64) error
	}{
		{
			name: "maintenance",
			remove: func(_ context.Context, st *store.Store, sourceID int64) error {
				return st.RemoveSource(sourceID)
			},
		},
		{
			name: "serialized",
			remove: func(ctx context.Context, st *store.Store, sourceID int64) error {
				_, _, err := st.RemoveSourceSerialized(ctx, sourceID)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			ctx := t.Context()

			observed, err := f.Store.EnsureParticipantByIdentifier(
				"example", "observed", "Observed",
			)
			require.NoError(err)
			other, err := f.Store.EnsureParticipantByIdentifier(
				"example", "other", "Other",
			)
			require.NoError(err)
			observation, err := f.Store.RecordContactObservationContext(
				ctx, observed, store.ParticipantContactObservationInput{
					SourceID: &f.Source.ID, AddressKind: store.ContactAddressEmail,
					OriginalValue: "observed@example.org",
					Envelope: store.ValueEnvelopeInput{
						Source: store.ProvenanceArchiveObservation,
					},
				},
			)
			require.NoError(err)
			candidate, created, err := f.Store.UpsertIdentityMatchCandidateContext(
				ctx, store.IdentityMatchCandidateInput{
					LeftKind:  store.IdentityMatchObservation,
					LeftID:    observation.Observation.Envelope.ID,
					RightKind: store.IdentityMatchParticipant,
					RightID:   other,
					Basis:     store.IdentityMatchEmail,
					State:     store.IdentityMatchStateCandidate,
					Source:    store.ProvenanceArchiveObservation,
				},
			)
			require.NoError(err)
			require.True(created)
			_, err = f.Store.AddIdentityMatchEvidenceContext(
				ctx, candidate.ID, store.IdentityMatchEvidenceInput{
					EvidenceKind: "display_name",
					Source:       store.ProvenanceArchiveObservation,
				},
			)
			require.NoError(err)

			require.NoError(test.remove(ctx, f.Store, f.Source.ID))

			for table, want := range map[string]int{
				"participant_contact_observations": 0,
				"identity_match_candidates":        0,
				"identity_match_evidence":          0,
			} {
				var got int
				err := f.Store.DB().QueryRow(
					"SELECT COUNT(*) FROM " + table,
				).Scan(&got)
				require.NoError(err, "count %s", table)
				assert.Equal(want, got, table)
			}
		})
	}
}

func TestStore_RemoveSourceRecomputesGeneratedIdentityMatchSupport(t *testing.T) {
	for _, test := range []struct {
		name   string
		remove func(context.Context, *store.Store, int64) error
	}{
		{
			name: "maintenance",
			remove: func(_ context.Context, st *store.Store, sourceID int64) error {
				return st.RemoveSource(sourceID)
			},
		},
		{
			name: "serialized",
			remove: func(ctx context.Context, st *store.Store, sourceID int64) error {
				_, _, err := st.RemoveSourceSerialized(ctx, sourceID)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Run("unsupported suggestion and evidence are removed", func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				ctx := t.Context()
				f := storetest.New(t)
				left := f.EnsureParticipant("source-support-left@example.org", "Left", "example.org")
				right := f.EnsureParticipant("source-support-right@example.org", "Right", "example.org")
				normalized := "provider:source-support"
				candidate, created, err := f.Store.UpsertIdentityMatchCandidateContext(
					ctx, store.IdentityMatchCandidateInput{
						LeftKind: store.IdentityMatchParticipant, LeftID: left,
						RightKind: store.IdentityMatchParticipant, RightID: right,
						Basis:           store.IdentityMatchStableProviderID,
						NormalizedValue: &normalized,
						State:           store.IdentityMatchStateCandidate,
						Source:          store.ProvenanceArchiveObservation,
						SourceID:        &f.Source.ID,
					},
				)
				require.NoError(err)
				require.True(created)
				_, err = f.Store.AddIdentityMatchEvidenceContext(
					ctx, candidate.ID, store.IdentityMatchEvidenceInput{
						EvidenceKind: "stable_provider_id",
						Detail:       new("source support"),
						Source:       store.ProvenanceArchiveObservation,
						SourceID:     &f.Source.ID,
					},
				)
				require.NoError(err)

				require.NoError(test.remove(ctx, f.Store, f.Source.ID))
				candidates, err := f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
				require.NoError(err)
				assert.Empty(candidates)
				var evidenceCount int
				require.NoError(f.Store.DB().QueryRow(
					`SELECT COUNT(*) FROM identity_match_evidence`,
				).Scan(&evidenceCount))
				assert.Zero(evidenceCount)
			})

			t.Run("remaining source support preserves suggestion and evidence", func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				ctx := t.Context()
				f := storetest.New(t)
				other, err := f.Store.GetOrCreateSource("beeper", "other-support")
				require.NoError(err)
				left := f.EnsureParticipant("remaining-support-left@example.org", "Left", "example.org")
				right := f.EnsureParticipant("remaining-support-right@example.org", "Right", "example.org")
				normalized := "provider:remaining-support"
				candidate, _, err := f.Store.UpsertIdentityMatchCandidateContext(
					ctx, store.IdentityMatchCandidateInput{
						LeftKind: store.IdentityMatchParticipant, LeftID: left,
						RightKind: store.IdentityMatchParticipant, RightID: right,
						Basis:           store.IdentityMatchStableProviderID,
						NormalizedValue: &normalized,
						State:           store.IdentityMatchStateCandidate,
						Source:          store.ProvenanceArchiveObservation,
						SourceID:        &f.Source.ID,
					},
				)
				require.NoError(err)
				_, _, err = f.Store.UpsertIdentityMatchCandidateContext(
					ctx, store.IdentityMatchCandidateInput{
						LeftKind: store.IdentityMatchParticipant, LeftID: left,
						RightKind: store.IdentityMatchParticipant, RightID: right,
						Basis:           store.IdentityMatchStableProviderID,
						NormalizedValue: &normalized,
						State:           store.IdentityMatchStateCandidate,
						Source:          store.ProvenanceArchiveObservation,
						SourceID:        &other.ID,
					},
				)
				require.NoError(err)
				recordStableProviderObservationPair(
					ctx, t, f.Store, other.ID, left, right, normalized,
				)
				firstEvidence, err := f.Store.AddIdentityMatchEvidenceContext(
					ctx, candidate.ID, store.IdentityMatchEvidenceInput{
						EvidenceKind: "stable_provider_id", Detail: &normalized,
						Source: store.ProvenanceArchiveObservation, SourceID: &f.Source.ID,
					},
				)
				require.NoError(err)
				require.NoError(f.Store.AttachIdentityMatchEvidenceSourceContext(
					ctx, firstEvidence.ID, other.ID,
				))

				require.NoError(test.remove(ctx, f.Store, f.Source.ID))
				kept, err := f.Store.GetIdentityMatchCandidateContext(ctx, candidate.ID)
				require.NoError(err)
				assert.Equal(store.IdentityMatchStateCandidate, kept.State)
				assert.Len(kept.Evidence, 1,
					"a doubly-supported generated evidence row remains")
			})

			t.Run("system link is withdrawn and user decision is preserved", func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				ctx := t.Context()
				f := storetest.New(t)
				left := f.EnsureParticipant("link-support-left@example.org", "Left", "example.org")
				right := f.EnsureParticipant("link-support-right@example.org", "Right", "example.org")
				normalized := "provider:link-support"
				candidate, _, err := f.Store.UpsertIdentityMatchCandidateContext(
					ctx, store.IdentityMatchCandidateInput{
						LeftKind: store.IdentityMatchParticipant, LeftID: left,
						RightKind: store.IdentityMatchParticipant, RightID: right,
						Basis:           store.IdentityMatchStableProviderID,
						NormalizedValue: &normalized,
						State:           store.IdentityMatchStateCandidate,
						Source:          store.ProvenanceArchiveObservation,
						SourceID:        &f.Source.ID,
					},
				)
				require.NoError(err)
				_, _, err = f.Store.AcceptIdentityMatchCandidateContext(
					ctx, candidate.ID, "system", nil,
				)
				require.NoError(err)
				require.NoError(test.remove(ctx, f.Store, f.Source.ID))
				var linkCount int
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
					`SELECT COUNT(*) FROM participant_links WHERE participant_a = ? AND participant_b = ?`,
				), min(left, right), max(left, right),
				).Scan(&linkCount))
				assert.Zero(linkCount)
				_, err = f.Store.GetIdentityMatchCandidateContext(ctx, candidate.ID)
				require.ErrorIs(err, store.ErrIdentityMatchNotFound)

				userFixture := storetest.New(t)
				userLeft := userFixture.EnsureParticipant("user-link-left@example.org", "Left", "example.org")
				userRight := userFixture.EnsureParticipant("user-link-right@example.org", "Right", "example.org")
				userNormalized := "provider:user-link-support"
				userCandidate, _, err := userFixture.Store.UpsertIdentityMatchCandidateContext(
					ctx, store.IdentityMatchCandidateInput{
						LeftKind: store.IdentityMatchParticipant, LeftID: userLeft,
						RightKind: store.IdentityMatchParticipant, RightID: userRight,
						Basis:           store.IdentityMatchStableProviderID,
						NormalizedValue: &userNormalized,
						State:           store.IdentityMatchStateCandidate,
						Source:          store.ProvenanceArchiveObservation,
						SourceID:        &userFixture.Source.ID,
					},
				)
				require.NoError(err)
				_, _, err = userFixture.Store.AcceptIdentityMatchCandidateContext(
					ctx, userCandidate.ID, "user", nil,
				)
				require.NoError(err)
				require.NoError(test.remove(ctx, userFixture.Store, userFixture.Source.ID))
				remaining, err := userFixture.Store.GetIdentityMatchCandidateContext(ctx, userCandidate.ID)
				require.NoError(err)
				assert.Equal(store.IdentityMatchStateAccepted, remaining.State)
				var userLinkCount int
				require.NoError(userFixture.Store.DB().QueryRow(userFixture.Store.Rebind(
					`SELECT COUNT(*) FROM participant_links WHERE participant_a = ? AND participant_b = ?`,
				), min(userLeft, userRight), max(userLeft, userRight),
				).Scan(&userLinkCount))
				assert.Equal(1, userLinkCount)
			})

			t.Run("unsupported owner transfers to another accepted candidate", func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				ctx := t.Context()
				f := storetest.New(t)
				other, err := f.Store.GetOrCreateSource("beeper", "replacement-support")
				require.NoError(err)
				left := f.EnsureParticipant("transfer-left@example.org", "Left", "example.org")
				right := f.EnsureParticipant("transfer-right@example.org", "Right", "example.org")
				firstValue := "provider:first-support"
				first, _, err := f.Store.UpsertIdentityMatchCandidateContext(ctx,
					store.IdentityMatchCandidateInput{
						LeftKind: store.IdentityMatchParticipant, LeftID: left,
						RightKind: store.IdentityMatchParticipant, RightID: right,
						Basis: store.IdentityMatchStableProviderID, NormalizedValue: &firstValue,
						State:  store.IdentityMatchStateCandidate,
						Source: store.ProvenanceArchiveObservation, SourceID: &f.Source.ID,
					})
				require.NoError(err)
				recordStableProviderObservationPair(
					ctx, t, f.Store, f.Source.ID, left, right, firstValue,
				)
				secondValue := "provider:replacement-support"
				second, _, err := f.Store.UpsertIdentityMatchCandidateContext(ctx,
					store.IdentityMatchCandidateInput{
						LeftKind: store.IdentityMatchParticipant, LeftID: left,
						RightKind: store.IdentityMatchParticipant, RightID: right,
						Basis: store.IdentityMatchStableProviderID, NormalizedValue: &secondValue,
						State:  store.IdentityMatchStateCandidate,
						Source: store.ProvenanceArchiveObservation, SourceID: &other.ID,
					})
				require.NoError(err)
				recordStableProviderObservationPair(
					ctx, t, f.Store, other.ID, left, right, secondValue,
				)
				_, _, err = f.Store.AcceptIdentityMatchCandidateContext(ctx, first.ID, "system", nil)
				require.NoError(err)
				_, _, err = f.Store.AcceptIdentityMatchCandidateContext(ctx, second.ID, "system", nil)
				require.NoError(err)

				require.NoError(test.remove(ctx, f.Store, f.Source.ID))
				var owner int64
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
					`SELECT identity_match_candidate_id FROM participant_links
					 WHERE participant_a = ? AND participant_b = ?`),
					min(left, right), max(left, right)).Scan(&owner))
				assert.Equal(second.ID, owner)
				assert.True(linkedPair(t, f.Store, left, right))

				require.NoError(test.remove(ctx, f.Store, other.ID))
				assert.False(linkedPair(t, f.Store, left, right))
			})

			t.Run("accepted assertion is reapplied after owned edge removal", func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				ctx := t.Context()
				f := storetest.New(t)
				secondSource, err := f.Store.GetOrCreateSource("beeper", "forest-second")
				require.NoError(err)
				thirdSource, err := f.Store.GetOrCreateSource("beeper", "forest-third")
				require.NoError(err)
				a := f.EnsureParticipant("forest-a@example.org", "A", "example.org")
				b := f.EnsureParticipant("forest-b@example.org", "B", "example.org")
				c := f.EnsureParticipant("forest-c@example.org", "C", "example.org")

				makeAccepted := func(left, right int64, value string, sourceID int64) *store.IdentityMatchCandidate {
					candidate, _, candidateErr := f.Store.UpsertIdentityMatchCandidateContext(
						ctx, store.IdentityMatchCandidateInput{
							LeftKind: store.IdentityMatchParticipant, LeftID: left,
							RightKind: store.IdentityMatchParticipant, RightID: right,
							Basis: store.IdentityMatchStableProviderID, NormalizedValue: &value,
							State:  store.IdentityMatchStateCandidate,
							Source: store.ProvenanceArchiveObservation, SourceID: &sourceID,
						})
					require.NoError(candidateErr)
					_, _, candidateErr = f.Store.AcceptIdentityMatchCandidateContext(
						ctx, candidate.ID, "system", nil)
					require.NoError(candidateErr)
					recordStableProviderObservationPair(
						ctx, t, f.Store, sourceID, left, right, value,
					)
					return candidate
				}

				first := makeAccepted(a, b, "provider:forest-first", f.Source.ID)
				_ = makeAccepted(a, c, "provider:forest-second", secondSource.ID)
				crossing := makeAccepted(b, c, "provider:forest-crossing", thirdSource.ID)
				assert.True(linkedPair(t, f.Store, b, c), "precondition: all three participants linked")
				var directCrossing int
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
					`SELECT COUNT(*) FROM participant_links
					 WHERE participant_a = ? AND participant_b = ?`),
					min(b, c), max(b, c)).Scan(&directCrossing))
				assert.Zero(directCrossing,
					"the crossing candidate must initially rely on the indirect path")

				require.NoError(test.remove(ctx, f.Store, f.Source.ID))
				_, err = f.Store.GetIdentityMatchCandidateContext(ctx, first.ID)
				require.ErrorIs(err, store.ErrIdentityMatchNotFound)
				assert.True(linkedPair(t, f.Store, b, c),
					"cleanup must reapply an accepted assertion that crosses the split")
				var owner int64
				require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
					`SELECT identity_match_candidate_id FROM participant_links
					 WHERE participant_a = ? AND participant_b = ?`),
					min(b, c), max(b, c)).Scan(&owner))
				assert.Equal(crossing.ID, owner)
			})
		})
	}
}

func TestStore_RemoveSourceRecomputesGeneratedObservationConflicts(t *testing.T) {
	for _, test := range []struct {
		kind  store.ContactAddressKind
		value string
	}{
		{kind: store.ContactAddressEmail, value: "shared@example.org"},
		{kind: store.ContactAddressPhone, value: "+1 202 555 0147"},
		{kind: store.ContactAddressUsername, value: "shared-user"},
		{kind: store.ContactAddressIMPP, value: "im:shared"},
		{kind: store.ContactAddressURL, value: "https://example.org/shared"},
		{kind: store.ContactAddressSocial, value: "social:shared"},
		{kind: store.ContactAddressCalendar, value: "calendar:shared"},
		{kind: store.ContactAddressContactURI, value: "contact:shared"},
		{kind: store.ContactAddressOrgDirectory, value: "directory:shared"},
		{kind: store.ContactAddressLanguage, value: "EN"},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			ctx := t.Context()
			left := f.EnsureParticipant("left@example.com", "Left", "example.com")
			right := f.EnsureParticipant("right@example.com", "Right", "example.com")

			for index, participantID := range []int64{left, right} {
				result, err := f.Store.RecordContactObservationContext(
					ctx, participantID, store.ParticipantContactObservationInput{
						SourceID: &f.Source.ID, AddressKind: test.kind,
						ProviderUserID: new(fmt.Sprintf("provider-%d", index)),
						OriginalValue:  test.value,
						Envelope: store.ValueEnvelopeInput{
							Source: store.ProvenanceArchiveObservation,
						},
					},
				)
				require.NoError(err)
				if participantID == right {
					assert.True(result.Conflicting)
					require.NotNil(result.CandidateID)
				}
			}

			candidates, err := f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
			require.NoError(err)
			require.Len(candidates, 1)
			assert.Equal(store.IdentityMatchParticipant, candidates[0].LeftKind)
			assert.Equal(store.IdentityMatchParticipant, candidates[0].RightKind)

			require.NoError(f.Store.RemoveSource(f.Source.ID))
			candidates, err = f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
			require.NoError(err)
			assert.Empty(candidates)
		})
	}
}

func TestStore_RemoveSourceKeepsGeneratedConflictWithOtherSourceSupport(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	left := f.EnsureParticipant("left@example.com", "Left", "example.com")
	right := f.EnsureParticipant("right@example.com", "Right", "example.com")
	otherSource, err := f.Store.GetOrCreateSource("gmail", "other@example.org")
	require.NoError(err)

	for _, sourceID := range []int64{f.Source.ID, otherSource.ID} {
		for _, participantID := range []int64{left, right} {
			_, err := f.Store.RecordContactObservationContext(
				ctx, participantID, store.ParticipantContactObservationInput{
					SourceID: &sourceID, AddressKind: store.ContactAddressEmail,
					OriginalValue: "shared@example.org",
					Envelope: store.ValueEnvelopeInput{
						Source: store.ProvenanceArchiveObservation,
					},
				},
			)
			require.NoError(err)
		}
	}

	require.NoError(f.Store.RemoveSource(f.Source.ID))
	candidates, err := f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1, "the second source still supports both endpoints")

	require.NoError(f.Store.RemoveSource(otherSource.ID))
	candidates, err = f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Empty(candidates)
}

func TestStore_RemoveUnrelatedSourceKeepsGeneratedUsernameConflict(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	supportSource, err := f.Store.GetOrCreateSource("beeper", "username-conflict-support")
	require.NoError(err)
	left := f.EnsureParticipant("username-conflict-left@example.org", "Left", "example.org")
	right := f.EnsureParticipant("username-conflict-right@example.org", "Right", "example.org")
	service := "slack"
	scopeKind := "workspace"
	scopeValue := "same-workspace"
	for index, participantID := range []int64{left, right} {
		result, recordErr := f.Store.RecordContactObservationContext(
			ctx, participantID, store.ParticipantContactObservationInput{
				SourceID:       &supportSource.ID,
				AddressKind:    store.ContactAddressUsername,
				ServiceSlug:    &service,
				ScopeKind:      &scopeKind,
				ScopeValue:     &scopeValue,
				ProviderUserID: new(fmt.Sprintf("username-conflict-provider-%d", index)),
				OriginalValue:  "shared-username",
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceArchiveObservation,
				},
			},
		)
		require.NoError(recordErr)
		if participantID == right {
			require.True(result.Conflicting)
		}
	}

	require.NoError(f.Store.RemoveSource(f.Source.ID))
	candidates, err := f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	require.Equal(store.IdentityMatchStateConflict, candidates[0].State)
}

func TestStore_RemoveUnrelatedSourceKeepsSupportedPolymorphicEvidence(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	supportSource, err := f.Store.GetOrCreateSource("beeper", "polymorphic-evidence-support")
	require.NoError(err)
	left := f.EnsureParticipant("polymorphic-left@example.org", "Left", "example.org")
	right := f.EnsureParticipant("polymorphic-right@example.org", "Right", "example.org")
	observation, err := f.Store.RecordContactObservationContext(
		ctx, left, store.ParticipantContactObservationInput{
			SourceID: &supportSource.ID, AddressKind: store.ContactAddressEmail,
			OriginalValue: "polymorphic-observation@example.org",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		},
	)
	require.NoError(err)
	candidate, _, err := f.Store.UpsertIdentityMatchCandidateContext(
		ctx, store.IdentityMatchCandidateInput{
			LeftKind:  store.IdentityMatchObservation,
			LeftID:    observation.Observation.Envelope.ID,
			RightKind: store.IdentityMatchParticipant,
			RightID:   right,
			Basis:     store.IdentityMatchEmail,
			State:     store.IdentityMatchStateCandidate,
			Source:    store.ProvenanceArchiveObservation,
			SourceID:  &supportSource.ID,
		},
	)
	require.NoError(err)
	evidence, err := f.Store.AddIdentityMatchEvidenceContext(
		ctx, candidate.ID, store.IdentityMatchEvidenceInput{
			EvidenceKind: "email", Detail: new("shared@example.org"),
			Source: store.ProvenanceArchiveObservation, SourceID: &supportSource.ID,
		},
	)
	require.NoError(err)

	require.NoError(f.Store.RemoveSource(f.Source.ID))
	reloaded, err := f.Store.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	require.Len(reloaded.Evidence, 1)
	require.Equal(evidence.ID, reloaded.Evidence[0].ID)
}

func TestStoreRemoveSourceDemotesPromotedObservationConflict(t *testing.T) {
	for name, remove := range map[string]func(context.Context, *store.Store, int64) error{
		"maintenance": func(_ context.Context, st *store.Store, sourceID int64) error {
			return st.RemoveSource(sourceID)
		},
		"serialized": func(ctx context.Context, st *store.Store, sourceID int64) error {
			_, _, err := st.RemoveSourceSerialized(ctx, sourceID)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			ctx := t.Context()
			left := f.EnsureParticipant("promoted-left@example.org", "Promoted Left", "example.org")
			right := f.EnsureParticipant("promoted-right@example.org", "Promoted Right", "example.org")
			normalized := "shared-promoted@example.org"
			notes := "preserve source-removal review"
			candidate, created, err := f.Store.UpsertIdentityMatchCandidateContext(
				ctx, store.IdentityMatchCandidateInput{
					LeftKind: store.IdentityMatchParticipant, LeftID: left,
					RightKind: store.IdentityMatchParticipant, RightID: right,
					Basis: store.IdentityMatchEmail, NormalizedValue: &normalized,
					State: store.IdentityMatchStateCandidate, Source: store.ProvenanceSystem,
					Notes: &notes,
				},
			)
			require.NoError(err)
			require.True(created)
			_, err = f.Store.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
				EvidenceKind: "system_review", Detail: new("preserve source-removal evidence"),
				Source: store.ProvenanceSystem,
			})
			require.NoError(err)

			for index, participantID := range []int64{left, right} {
				_, err := f.Store.RecordContactObservationContext(
					ctx, participantID, store.ParticipantContactObservationInput{
						SourceID: &f.Source.ID, AddressKind: store.ContactAddressEmail,
						ProviderUserID: new(fmt.Sprintf("promoted-provider-%d", index)),
						OriginalValue:  normalized,
						Envelope: store.ValueEnvelopeInput{
							Source: store.ProvenanceArchiveObservation,
						},
					},
				)
				require.NoError(err)
			}

			require.NoError(remove(ctx, f.Store, f.Source.ID))
			candidates, err := f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
			require.NoError(err)
			require.Len(candidates, 1)
			assert.Equal(candidate.ID, candidates[0].ID)
			assert.Equal(store.IdentityMatchStateCandidate, candidates[0].State)
			assert.Equal(store.ProvenanceSystem, candidates[0].Source)
			assert.Equal(&notes, candidates[0].Notes)
			require.Len(candidates[0].Evidence, 1)
			assert.Equal("system_review", candidates[0].Evidence[0].EvidenceKind)
		})
	}
}

func TestStore_RemoveSourceKeepsConflictsBetweenOtherParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	participants := []int64{
		f.EnsureParticipant("first@example.com", "First", "example.com"),
		f.EnsureParticipant("second@example.com", "Second", "example.com"),
		f.EnsureParticipant("third@example.com", "Third", "example.com"),
	}
	sources := []int64{f.Source.ID}
	for _, identifier := range []string{"second-source@example.org", "third-source@example.org"} {
		source, err := f.Store.GetOrCreateSource("gmail", identifier)
		require.NoError(err)
		sources = append(sources, source.ID)
	}

	for index, participantID := range participants {
		_, err := f.Store.RecordContactObservationContext(
			ctx, participantID, store.ParticipantContactObservationInput{
				SourceID: &sources[index], AddressKind: store.ContactAddressEmail,
				ProviderUserID: new(fmt.Sprintf("provider-%d", index)),
				OriginalValue:  "shared@example.org",
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceArchiveObservation,
				},
			},
		)
		require.NoError(err)
	}
	candidates, err := f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 3, "three participants require a complete conflict graph")

	require.NoError(f.Store.RemoveSource(sources[0]))
	candidates, err = f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.ElementsMatch(
		[]int64{participants[1], participants[2]},
		[]int64{candidates[0].LeftID, candidates[0].RightID},
	)
}

func TestStore_RemoveSource_CascadesConversations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Create message with body, raw, and recipients
	msgID := f.CreateMessage("msg-cascade-1")

	err := f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "body text", Valid: true},
		sql.NullString{},
	)
	require.NoError(err, "UpsertMessageBody")

	err = f.Store.UpsertMessageRaw(msgID, []byte("raw MIME data"))
	require.NoError(err, "UpsertMessageRaw")

	pid := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	err = f.Store.ReplaceMessageRecipients(
		msgID, "from", []int64{pid}, []string{"Sender"},
	)
	require.NoError(err, "ReplaceMessageRecipients")

	// Remove source
	err = f.Store.RemoveSource(f.Source.ID)
	require.NoError(err, "RemoveSource")

	// Verify conversations are gone
	var convCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM conversations WHERE source_id = ?`),
		f.Source.ID,
	).Scan(&convCount)
	require.NoError(err, "count conversations")
	assert.Equal(0, convCount, "conversation count")

	// Verify message_bodies are gone (cascaded via messages)
	var bodyCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM message_bodies WHERE message_id = ?`), msgID,
	).Scan(&bodyCount)
	require.NoError(err, "count message_bodies")
	assert.Equal(0, bodyCount, "message_bodies count")

	// Verify message_raw is gone (cascaded via messages)
	var rawCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM message_raw WHERE message_id = ?`), msgID,
	).Scan(&rawCount)
	require.NoError(err, "count message_raw")
	assert.Equal(0, rawCount, "message_raw count")

	// Verify message_recipients are gone (cascaded via messages)
	var recipCount int
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), msgID,
	).Scan(&recipCount)
	require.NoError(err, "count message_recipients")
	assert.Equal(0, recipCount, "message_recipients count")
}

func TestStore_RemoveSourceSerialized_NoActiveSync(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	f.CreateMessage("msg-1")

	had, removed, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	require.NoError(err, "RemoveSourceSerialized")
	assert.False(had, "hadActiveSync")
	assert.Zero(removed, "no packed mappings")

	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "source should be removed")
}

func TestStore_RemoveSourceSerialized_PreservesPendingOperation(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	fixture := storetest.New(t)
	const operationID = "pending-operation"

	_, err := fixture.Store.CreateSyncOperation(fixture.Source.ID, operationID)
	requirements.NoError(err)

	hadActiveSync, removed, err := fixture.Store.RemoveSourceSerialized(
		t.Context(), fixture.Source.ID,
	)
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)
	checks.True(hadActiveSync)
	checks.Zero(removed)

	source, err := fixture.Store.GetSourceByID(fixture.Source.ID)
	requirements.NoError(err)
	checks.Equal(fixture.Source.ID, source.ID)
	operation, err := fixture.Store.GetSyncOperation(operationID)
	requirements.NoError(err)
	checks.Equal("pending", operation.Status)
	requirements.NoError(fixture.Store.FinishSyncOperation(operationID, "failed"))
}

func TestStore_RemoveSourceSerialized_PackedLogicalGC(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	otherConv, err := f.Store.EnsureConversation(other.ID, "other-thread", "Other")
	require.NoError(err)
	otherMsg, err := f.Store.UpsertMessage(&store.Message{
		ConversationID: otherConv, SourceID: other.ID,
		SourceMessageID: "other-packed", MessageType: "email",
	})
	require.NoError(err)

	hash := func(prefix string) string { return prefix + strings.Repeat("0", 64-len(prefix)) }
	uniqueContent := hash("aa01")
	uniqueThumbnail := hash("bb02")
	sharedContentContent := hash("cc03")
	sharedThumbnailThumbnail := hash("dd04")
	sharedContentThumbnail := hash("ee05")
	sharedThumbnailContent := hash("ff06")

	add := func(msgID int64, filename, contentHash string) {
		require.NoError(f.Store.UpsertAttachment(msgID, filename, "application/octet-stream",
			contentHash[:2]+"/"+contentHash, contentHash, 10))
	}
	setThumbnail := func(msgID int64, contentHash, thumbnailHash string) {
		_, err := f.Store.DB().Exec(f.Store.Rebind(`
			UPDATE attachments SET thumbnail_hash = ?, thumbnail_path = ?
			WHERE message_id = ? AND content_hash = ?`),
			thumbnailHash, thumbnailHash[:2]+"/"+thumbnailHash, msgID, contentHash)
		require.NoError(err)
	}

	msgA := f.CreateMessage("msg-packed")
	add(msgA, "unique.bin", uniqueContent)
	setThumbnail(msgA, uniqueContent, uniqueThumbnail)
	add(msgA, "shared-cc.bin", sharedContentContent)
	carrierATT := hash("1007")
	add(msgA, "carrier-att.bin", carrierATT)
	setThumbnail(msgA, carrierATT, sharedThumbnailThumbnail)
	add(msgA, "shared-ct.bin", sharedContentThumbnail)
	carrierATC := hash("1108")
	add(msgA, "carrier-atc.bin", carrierATC)
	setThumbnail(msgA, carrierATC, sharedThumbnailContent)

	add(otherMsg, "shared-cc.bin", sharedContentContent)
	carrierBTT := hash("1209")
	add(otherMsg, "carrier-btt.bin", carrierBTT)
	setThumbnail(otherMsg, carrierBTT, sharedThumbnailThumbnail)
	carrierBCT := hash("130a")
	add(otherMsg, "carrier-bct.bin", carrierBCT)
	setThumbnail(otherMsg, carrierBCT, sharedContentThumbnail)
	add(otherMsg, "shared-tc.bin", sharedThumbnailContent)

	const packID = "01hzy3v7q8r9s0t1a2v3w4x5r1"
	packedHashes := []string{
		uniqueContent, uniqueThumbnail, sharedContentContent,
		sharedThumbnailThumbnail, sharedContentThumbnail, sharedThumbnailContent,
	}
	entries := make([]store.PackIndexEntry, 0, len(packedHashes))
	for i, packedHash := range packedHashes {
		entries = append(entries, store.PackIndexEntry{
			BlobHash: packedHash, PackID: packID, Offset: int64(i * 10),
			StoredLen: 10, RawLen: 10,
		})
	}
	require.NoError(f.Store.RecordPackedBlobs(store.PackRecord{
		PackID: packID, EntryCount: int64(len(entries)), StoredBytes: int64(len(entries) * 10),
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, entries))

	attachmentsDir := t.TempDir()
	loosePath := filepath.Join(attachmentsDir, uniqueContent[:2], uniqueContent)
	require.NoError(os.MkdirAll(filepath.Dir(loosePath), 0o700))
	require.NoError(os.WriteFile(loosePath, []byte("crash leftover"), 0o600))

	had, removed, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	require.NoError(err)
	assert.False(had)
	assert.Equal(int64(2), removed, "unique content and thumbnail mappings are deleted")

	src, err := f.Store.GetSourceByIdentifier(f.Source.Identifier)
	require.ErrorIs(err, store.ErrSourceNotFound)
	assert.Nil(src)

	for _, removedHash := range []string{uniqueContent, uniqueThumbnail} {
		entry, err := f.Store.GetAttachmentPackEntry(removedHash)
		require.NoError(err)
		assert.Nil(entry, "%s is logically deleted", removedHash)
	}
	for _, sharedHash := range []string{
		sharedContentContent, sharedThumbnailThumbnail,
		sharedContentThumbnail, sharedThumbnailContent,
	} {
		entry, err := f.Store.GetAttachmentPackEntry(sharedHash)
		require.NoError(err)
		assert.NotNil(entry, "%s remains shared through either hash column", sharedHash)
	}

	bs, err := attachmentstore.New(store.NewPackCatalog(f.Store), attachmentsDir)
	require.NoError(err)
	defer func() { require.NoError(bs.Close()) }()
	_, _, err = bs.Open(uniqueContent)
	require.ErrorIs(err, fs.ErrNotExist,
		"logical deletion rejects even a canonical loose crash leftover")
	assert.FileExists(loosePath, "file cleanup remains best effort and separate from logical GC")
}

func TestStore_RemoveSourceSerialized_PreservesPackedCaseAliasReferences(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(string) string
	}{
		{name: "URL only", path: func(hash string) string {
			return "HTTPS://cdn.example.com/" + hash
		}},
		{name: "empty path only", path: func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			if !f.Store.IsPostgreSQL() {
				f.Store.DB().SetMaxOpenConns(1)
				f.Store.DB().SetMaxIdleConns(1)
				_, err := f.Store.DB().Exec(`PRAGMA case_sensitive_like = ON`)
				require.NoError(err)
			}

			hash := "ab" + strings.Repeat("1", 62)
			uppercase := strings.ToUpper(hash)
			removedMessage := f.CreateMessage("msg-packed-case-alias-removed")
			require.NoError(f.Store.UpsertAttachment(removedMessage, "removed.bin",
				"application/octet-stream", hash[:2]+"/"+hash, hash, 10))

			survivor, err := f.Store.GetOrCreateSource("gmail", "survivor@example.com")
			require.NoError(err)
			survivorConversation, err := f.Store.EnsureConversation(
				survivor.ID, "case-alias-thread", "Case Alias Thread")
			require.NoError(err)
			survivorMessage, err := f.Store.UpsertMessage(&store.Message{
				ConversationID:  survivorConversation,
				SourceID:        survivor.ID,
				SourceMessageID: "msg-packed-case-alias-survivor",
				MessageType:     "email",
				SizeEstimate:    10,
			})
			require.NoError(err)
			require.NoError(f.Store.UpsertAttachment(survivorMessage, "survivor.bin",
				"application/octet-stream", tc.path(uppercase), uppercase, 10))

			const packID = "01hzy3v7q8r9s0t1a2v3w4x5r3"
			require.NoError(f.Store.RecordPackedBlobs(store.PackRecord{
				PackID: packID, EntryCount: 1, StoredBytes: 10,
				CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
			}, []store.PackIndexEntry{{
				BlobHash: hash, PackID: packID, StoredLen: 10, RawLen: 10,
			}}))

			had, removed, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)

			require.NoError(err)
			assert.False(had)
			assert.Zero(removed, "the surviving uppercase alias keeps the canonical mapping live")
			entry, err := f.Store.GetAttachmentPackEntry(hash)
			require.NoError(err)
			require.NotNil(entry)
			assert.Equal(packID, entry.PackID)
			for _, requested := range []string{hash, uppercase} {
				loc, err := f.Store.ResolveAttachmentBlob(requested)
				require.NoError(err)
				assert.True(loc.Referenced)
				require.NotNil(loc.Pack)
				assert.Equal(packID, loc.Pack.PackID)
			}
		})
	}
}

func TestStore_RemoveSourceSerialized_PackedRollbackOnSourceDeleteFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	hash := "aa" + strings.Repeat("1", 62)
	msgID := f.CreateMessage("msg-packed-rollback")
	require.NoError(f.Store.UpsertAttachment(msgID, "a.pdf", "application/pdf",
		hash[:2]+"/"+hash, hash, 10))
	const packID = "01hzy3v7q8r9s0t1a2v3w4x5r2"
	require.NoError(f.Store.RecordPackedBlobs(store.PackRecord{
		PackID: packID, EntryCount: 1, StoredBytes: 10,
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []store.PackIndexEntry{{BlobHash: hash, PackID: packID, StoredLen: 10, RawLen: 10}}))

	if f.Store.IsPostgreSQL() {
		_, err := f.Store.DB().Exec(`
			CREATE FUNCTION force_source_delete_failure() RETURNS trigger AS $$
			BEGIN
			    RAISE EXCEPTION 'forced source delete failure';
			END;
			$$ LANGUAGE plpgsql`)
		require.NoError(err)
		_, err = f.Store.DB().Exec(`
			CREATE TRIGGER force_source_delete_failure
			BEFORE DELETE ON sources FOR EACH ROW
			EXECUTE FUNCTION force_source_delete_failure()`)
		require.NoError(err)
	} else {
		_, err := f.Store.DB().Exec(`
			CREATE TRIGGER force_source_delete_failure
			BEFORE DELETE ON sources
			BEGIN
			    SELECT RAISE(ABORT, 'forced source delete failure');
			END`)
		require.NoError(err)
	}

	_, _, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	require.ErrorContains(err, "forced source delete failure")
	src, err := f.Store.GetSourceByIdentifier(f.Source.Identifier)
	require.NoError(err, "failed transaction retains source")
	assert.NotNil(src)
	entry, err := f.Store.GetAttachmentPackEntry(hash)
	require.NoError(err)
	assert.NotNil(entry, "failed transaction retains every packed mapping")
}

func TestStore_RemoveSourceSerialized_ActiveSyncSameSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	f.CreateMessage("msg-1")
	// Active sync on the source being removed — this row would be cascaded
	// by the DELETE. The serialized check must still observe it.
	f.StartSync()

	had, removed, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	require.NoError(err, "RemoveSourceSerialized")
	assert.True(had, "hadActiveSync should be true for sync on removed source")
	assert.Zero(removed)

	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "source should still be removed even when sync was active")
}

func TestStore_RemoveSourceSerialized_ActiveSyncOtherSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Create a second source with its own running sync.
	otherSrc, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "create other source")
	_, err = f.Store.StartSync(otherSrc.ID, "full")
	require.NoError(err, "start other sync")

	had, removed, err := f.Store.RemoveSourceSerialized(context.Background(), f.Source.ID)
	require.NoError(err, "RemoveSourceSerialized")
	assert.True(had, "hadActiveSync should be true for sync on another source")
	assert.Zero(removed)

	// Original source is gone.
	src, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "test source should be removed")

	// Other source (with the active sync) is untouched.
	other, err := f.Store.GetSourceByIdentifier("other@example.com")
	require.NoError(err, "GetSourceByIdentifier other")
	assert.NotNil(other, "other source should remain")
}

func TestStore_RemoveSourceSerialized_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	_, _, err := st.RemoveSourceSerialized(context.Background(), 99999)
	require.Error(t, err, "RemoveSourceSerialized should error for nonexistent ID")
}

func TestStore_AttachmentPathsUniqueToSource(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	// Create a second source with its own conversation.
	otherSrc, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "create other source")
	otherConv, err := f.Store.EnsureConversation(otherSrc.ID, "other-thread", "Other")
	require.NoError(err, "ensure other conv")
	otherMsgID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  otherConv,
		SourceID:        otherSrc.ID,
		SourceMessageID: "other-msg-1",
		MessageType:     "email",
	})
	require.NoError(err, "create other message")

	// Attachment unique to the default source.
	uniqueMsg := f.CreateMessage("msg-unique")
	err = f.Store.UpsertAttachment(uniqueMsg, "u.pdf", "application/pdf",
		"aa/uniquehash", "uniquehash", 10)
	require.NoError(err, "upsert unique attachment")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attachments
		SET thumbnail_hash = ?, thumbnail_path = ?
		WHERE message_id = ? AND content_hash = ?`),
		"uniquethumbhash", "dd/uniquethumbhash", uniqueMsg, "uniquehash")
	require.NoError(err, "set unique thumbnail")

	// Attachment shared with another source (same content_hash).
	sharedMsg := f.CreateMessage("msg-shared")
	err = f.Store.UpsertAttachment(sharedMsg, "s.pdf", "application/pdf",
		"bb/sharedhash", "sharedhash", 20)
	require.NoError(err, "upsert shared attachment in default source")
	err = f.Store.UpsertAttachment(otherMsgID, "s.pdf", "application/pdf",
		"bb/sharedhash", "sharedhash", 20)
	require.NoError(err, "upsert shared attachment in other source")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attachments
		SET thumbnail_hash = ?, thumbnail_path = ?
		WHERE message_id = ? AND content_hash = ?`),
		"crosshash", "ee/crosshash", sharedMsg, "sharedhash")
	require.NoError(err, "set cross-type shared thumbnail")
	err = f.Store.UpsertAttachment(otherMsgID, "cross.pdf", "application/pdf",
		"ee/crosshash", "crosshash", 20)
	require.NoError(err, "share default-source thumbnail as other-source content")

	// Attachment with NULL content_hash (must be excluded).
	nullHashMsg := f.CreateMessage("msg-null-hash")
	_, err = f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		 VALUES (?, 'n.pdf', 'application/pdf', 'cc/x', NULL, 30, CURRENT_TIMESTAMP)`),
		nullHashMsg,
	)
	require.NoError(err, "insert null-hash attachment")

	// Attachment with empty storage_path (must be excluded).
	emptyPathMsg := f.CreateMessage("msg-empty-path")
	err = f.Store.UpsertAttachment(emptyPathMsg, "e.pdf", "application/pdf",
		"", "emptypathhash", 40)
	require.NoError(err, "upsert empty-path attachment")

	// URL-backed attachment rows are links, not local files to clean up.
	urlBackedMsg := f.CreateMessage("msg-url-backed")
	_, err = f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		 VALUES (?, 'deck.pptx', 'reference', 'https://sp/deck.pptx', '', 0, CURRENT_TIMESTAMP)`),
		urlBackedMsg,
	)
	require.NoError(err, "insert URL-backed attachment")

	// Two messages in the default source referencing the same unique hash
	// should collapse to a single storage_path in the result.
	dupMsg := f.CreateMessage("msg-dup-hash")
	err = f.Store.UpsertAttachment(dupMsg, "u.pdf", "application/pdf",
		"aa/uniquehash", "uniquehash", 10)
	require.NoError(err, "upsert duplicate-of-unique attachment")

	paths, err := f.Store.AttachmentPathsUniqueToSource(f.Source.ID)
	require.NoError(err, "AttachmentPathsUniqueToSource")

	require.Len(paths, 2, "paths: %v", paths)
	got := testutil.MakeSet(paths...)
	assert.True(t, got["aa/uniquehash"], "unique content path missing: %v", paths)
	assert.True(t, got["dd/uniquethumbhash"], "unique thumbnail path missing: %v", paths)
	assert.False(t, got["ee/crosshash"], "cross-type shared thumbnail must be preserved: %v", paths)
}

func TestStore_GetSourceByID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	got, err := f.Store.GetSourceByID(f.Source.ID)
	require.NoError(err, "GetSourceByID")
	require.NotNil(got, "expected non-nil source")
	assert.Equal(f.Source.ID, got.ID, "ID")
	assert.Equal(f.Source.Identifier, got.Identifier, "Identifier")
}

func TestStore_GetSourceByID_NotFound(t *testing.T) {
	f := storetest.New(t)

	_, err := f.Store.GetSourceByID(99999)
	require.Error(t, err, "expected error for non-existent ID")
}

func TestStore_IsAttachmentPathReferenced(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-ref-1")
	err := f.Store.UpsertAttachment(msgID, "a.pdf", "application/pdf",
		"aa/hash1", "hash1", 10)
	require.NoError(err, "UpsertAttachment")

	referenced, err := f.Store.IsAttachmentPathReferenced("aa/hash1")
	require.NoError(err, "IsAttachmentPathReferenced (hit)")
	assert.True(referenced, "expected true for referenced path")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attachments SET thumbnail_path = ?
		WHERE message_id = ? AND content_hash = ?`),
		"thumbs/hash1", msgID, "hash1")
	require.NoError(err, "set thumbnail path")
	referenced, err = f.Store.IsAttachmentPathReferenced("thumbs/hash1")
	require.NoError(err, "IsAttachmentPathReferenced (thumbnail hit)")
	assert.True(referenced, "expected thumbnail path to count as referenced")

	referenced, err = f.Store.IsAttachmentPathReferenced("zz/nothere")
	require.NoError(err, "IsAttachmentPathReferenced (miss)")
	assert.False(referenced, "expected false for unreferenced path")
}

func TestInitSchema_MigratesOAuthAppColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Simulate a pre-migration database that lacks the oauth_app column.
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	// Create the sources table WITHOUT the oauth_app column,
	// matching the schema as it existed before this feature.
	_, err = st.DB().Exec(`
		CREATE TABLE IF NOT EXISTS sources (
			id INTEGER PRIMARY KEY,
			source_type TEXT NOT NULL,
			identifier TEXT NOT NULL,
			display_name TEXT,
			google_user_id TEXT UNIQUE,
			last_sync_at DATETIME,
			sync_cursor TEXT,
			sync_config JSON,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(source_type, identifier)
		)
	`)
	require.NoError(err, "create legacy sources table")

	// Insert a row into the legacy table.
	_, err = st.DB().Exec(`
		INSERT INTO sources (source_type, identifier, display_name)
		VALUES ('gmail', 'legacy@example.com', 'Legacy User')
	`)
	require.NoError(err, "insert legacy source")

	// Run InitSchema — this should migrate the table by adding oauth_app.
	require.NoError(st.InitSchema(), "InitSchema on legacy DB")

	// Verify GetSourcesByIdentifier works (reads oauth_app column).
	sources, err := st.GetSourcesByIdentifier("legacy@example.com")
	require.NoError(err, "GetSourcesByIdentifier after migration")
	require.Len(sources, 1)
	assert.False(sources[0].OAuthApp.Valid,
		"OAuthApp should be NULL for legacy row, got %q", sources[0].OAuthApp.String)

	// Verify GetSourcesByDisplayName works (also reads oauth_app column).
	sources, err = st.GetSourcesByDisplayName("Legacy User")
	require.NoError(err, "GetSourcesByDisplayName after migration")
	require.Len(sources, 1)

	// Verify oauth_app can be written and read back.
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE sources SET oauth_app = ? WHERE identifier = ?`),
		"acme", "legacy@example.com",
	)
	require.NoError(err, "update oauth_app")

	sources, err = st.GetSourcesByIdentifier("legacy@example.com")
	require.NoError(err, "GetSourcesByIdentifier after update")
	assert.True(sources[0].OAuthApp.Valid, "OAuthApp should be valid after update")
	assert.Equal("acme", sources[0].OAuthApp.String, "OAuthApp value")
}

// TestInitSchema_AddsDeletedAtToLegacyMessagesTable verifies the
// upgrade-path migration: a database whose `messages` table already has
// every other column the embedded schema indexes reference, but is
// missing the dedup-hide column `deleted_at`, gets the column added by
// InitSchema. Without the ALTER, every read path that references
// `deleted_at` (LiveMessagesWhere, the dedup engine, the cache
// staleness check) fails on upgraded databases with "no such column".
func TestInitSchema_AddsDeletedAtToLegacyMessagesTable(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	// Build a messages table that has every column the embedded
	// schema's CREATE INDEX statements reference (sender_id,
	// deleted_from_source_at, message_type, …) but DOES NOT have the
	// new dedup-hide columns (`deleted_at`, `delete_batch_id`).
	// Approximates a legacy DB just before this branch landed.
	// metadata has been on messages since the first schema and the activity
	// queue trigger names it, so a legacy table must carry it too.
	_, err = st.DB().Exec(`
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL,
			source_message_id TEXT,
			conversation_id INTEGER,
			subject TEXT,
			snippet TEXT,
			metadata JSON,
			sent_at DATETIME,
			received_at DATETIME,
			internal_date DATETIME,
			size_estimate INTEGER,
			has_attachments BOOLEAN,
			is_from_me BOOLEAN,
			archived_at DATETIME,
			rfc822_message_id TEXT,
			sender_id INTEGER,
			message_type TEXT NOT NULL DEFAULT 'email',
			attachment_count INTEGER DEFAULT 0,
			deleted_from_source_at DATETIME
		)
	`)
	require.NoError(err, "create legacy messages table")

	_, err = st.DB().Exec(`
		INSERT INTO messages (id, source_id, source_message_id, sent_at)
		VALUES (1, 1, 'msg1', datetime('now'))
	`)
	require.NoError(err, "insert legacy message")

	// Run InitSchema — should add deleted_at and delete_batch_id via
	// ALTER TABLE migrations (and silently no-op the columns that
	// already exist, like deleted_from_source_at).
	require.NoError(st.InitSchema(), "InitSchema on legacy DB")

	// Confirm the canonical live-messages predicate runs without
	// "no such column": this is the failure mode codex flagged. The
	// query uses both deleted_at and deleted_from_source_at.
	var n int
	require.NoError(st.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE "+store.LiveMessagesWhere("", true),
	).Scan(&n), "post-migration live count")
	assert.Equal(t, 1, n, "post-migration live count")

	// Confirm delete_batch_id is also queryable post-migration so
	// DeleteAllDeduped's distinct-batch count works on upgraded DBs.
	_, err = st.DB().Exec(
		"SELECT COUNT(DISTINCT delete_batch_id) FROM messages",
	)
	require.NoError(err, "post-migration delete_batch_id query")
}
