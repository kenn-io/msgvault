package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXSourceInstanceInitializationIsImmutable(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	before, err := st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Empty(t, before.InstanceID)
	first, err := st.UpdateXControlContext(t.Context(), source.ID, before.Revision, store.XControlPatch{OwnerID: "1001", InitializeInstance: true})
	require.NoError(t, err)
	assert.Len(t, first.InstanceID, 64)
	decoded, err := hex.DecodeString(first.InstanceID)
	require.NoError(t, err)
	assert.Len(t, decoded, 32)
	reconnected, err := st.UpdateXControlContext(t.Context(), source.ID, first.Revision, store.XControlPatch{OwnerID: "1001", InitializeInstance: true})
	require.NoError(t, err)
	assert.Equal(t, first.InstanceID, reconnected.InstanceID)
	secondSource, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	second, err := st.UpdateXControlContext(t.Context(), secondSource.ID, 0, store.XControlPatch{OwnerID: "2001", InitializeInstance: true})
	require.NoError(t, err)
	assert.NotEqual(t, first.InstanceID, second.InstanceID)
	assert.Equal(t, before.OwnerID, reconnected.OwnerID)
	assert.Equal(t, before.Generation, reconnected.Generation)
	assert.Equal(t, before.Reservations, reconnected.Reservations)
}

func TestXRemovalRecordCapturesCompleteBoundedIdentitySet(t *testing.T) {
	st, source := xPostFixture(t)
	instance := strings.Repeat("a", 64)
	seedXRestoreInstance(t, st, source, instance)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "synthetic aurora payload", "100", "102"))
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,availability='removed',cleanup_pending=0 WHERE source_id=?`), source)
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	seedXRestoreInstance(t, st, other.ID, strings.Repeat("b", 64))
	_, err = st.GetOrCreateSource("gmail", "1001")
	require.NoError(t, err)
	uid, err := st.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	record, head, err := st.ExportXRemovalRecordContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, record.Version)
	assert.Equal(t, uid, record.ArchiveUID)
	require.Len(t, record.Sources, 2)
	assert.Equal(t, source, record.Sources[0].SourceID)
	assert.Equal(t, "1001", record.Sources[0].OwnerID)
	assert.Equal(t, instance, record.Sources[0].InstanceID)
	assert.Equal(t, []string{"100", "102"}, record.Sources[0].RemovedIDs)
	assert.Len(t, record.Sources[0].FinancialHead, 64)
	assert.Equal(t, record.Sources[0].FinancialHead, record.Sources[1].FinancialHead, "retention metadata does not change empty financial authority")
	assert.Equal(t, other.ID, record.Sources[1].SourceID)
	assert.Empty(t, record.Sources[1].RemovedIDs)
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	assert.Equal(t, hex.EncodeToString(digest[:]), head)
	require.NoError(t, store.ValidateXRemovalRecord(record, head, uid))
	for _, content := range []string{"aurora", "example_author", "Example Author", "access_token", "username", "text"} {
		assert.NotContains(t, string(encoded), content)
	}
}

func TestXRemovalRecordRejectsPendingOrUnboundSource(t *testing.T) {
	st, source := xPostFixture(t)
	_, _, err := st.ExportXRemovalRecordContext(t.Context())
	assert.Error(t, err, "a source without a verified immutable incarnation cannot authorize old snapshots")
	seedXRestoreInstance(t, st, source, strings.Repeat("a", 64))
	_, err = st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "synthetic pending payload"))
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,availability='removed',cleanup_pending=1 WHERE source_id=?`), source)
	require.NoError(t, err)
	_, _, err = st.ExportXRemovalRecordContext(t.Context())
	require.Error(t, err, "a pending operation cannot publish complete removal authority")
}

func TestXRemovalRecordRequiresIndependentCompleteHead(t *testing.T) {
	record := store.XRemovalRecord{Version: 3, ArchiveUID: strings.Repeat("c", 64), Sources: []store.XRemovalRecordSource{{SourceID: 42, OwnerID: "1001", InstanceID: strings.Repeat("a", 64), FinancialHead: strings.Repeat("e", 64), TargetHead: strings.Repeat("f", 64), RemovedIDs: []string{"100", "102"}}}}
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	head := hex.EncodeToString(digest[:])
	for _, tc := range []struct {
		name string
		edit func(*store.XRemovalRecord)
	}{
		{"shortened", func(r *store.XRemovalRecord) { r.Sources[0].RemovedIDs = []string{"100"} }},
		{"missing source", func(r *store.XRemovalRecord) { r.Sources = nil }},
		{"recreated source", func(r *store.XRemovalRecord) { r.Sources[0].InstanceID = strings.Repeat("b", 64) }},
		{"wrong owner", func(r *store.XRemovalRecord) { r.Sources[0].OwnerID = "2001" }},
		{"wrong source", func(r *store.XRemovalRecord) { r.Sources[0].SourceID = 43 }},
		{"duplicate ID", func(r *store.XRemovalRecord) { r.Sources[0].RemovedIDs = []string{"100", "100", "102"} }},
		{"noncanonical ID", func(r *store.XRemovalRecord) { r.Sources[0].RemovedIDs = []string{"0100", "102"} }},
		{"duplicate source", func(r *store.XRemovalRecord) { r.Sources = append(r.Sources, r.Sources[0]) }},
		{"old version", func(r *store.XRemovalRecord) { r.Version = 2 }},
		{"future version", func(r *store.XRemovalRecord) { r.Version = 4 }},
		{"missing financial head", func(r *store.XRemovalRecord) { r.Sources[0].FinancialHead = "" }},
		{"missing target head", func(r *store.XRemovalRecord) { r.Sources[0].TargetHead = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := record
			changed.Sources = slices.Clone(record.Sources)
			tc.edit(&changed)
			require.Error(t, store.ValidateXRemovalRecord(changed, head, record.ArchiveUID))
		})
	}
	t.Run("missing current head", func(t *testing.T) { require.Error(t, store.ValidateXRemovalRecord(record, "", record.ArchiveUID)) })
	t.Run("wrong archive", func(t *testing.T) {
		require.Error(t, store.ValidateXRemovalRecord(record, head, strings.Repeat("d", 64)))
	})
}

func seedXRestoreInstance(t *testing.T, st *store.Store, source int64, instance string) {
	t.Helper()
	state, err := st.LoadXControlContext(t.Context(), source)
	require.NoError(t, err)
	state.InstanceID = instance
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET sync_cursor=? WHERE id=?`), string(encoded), source)
	require.NoError(t, err)
}

func TestXRemovalRecordMatchesRestoredSourceAndKnownSuppressions(t *testing.T) {
	st, source := xPostFixture(t)
	instance := strings.Repeat("a", 64)
	seedXRestoreInstance(t, st, source, instance)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "synthetic aurora payload", "100", "102"))
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,availability='removed',cleanup_pending=1 WHERE source_id=?`), source)
	require.NoError(t, err)
	uid, err := st.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	// Capture completed current authority, then restore the pending scratch
	// flag to prove validation allows the same local cleanup to resume.
	_, err = st.DB().Exec(st.Rebind(`UPDATE x_post_versions SET cleanup_pending=0 WHERE source_id=?`), source)
	require.NoError(t, err)
	record, _, err := st.ExportXRemovalRecordContext(t.Context())
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE x_post_versions SET cleanup_pending=1 WHERE source_id=?`), source)
	require.NoError(t, err)
	assert.Equal(t, uid, record.ArchiveUID)
	head := xRestoreRecordHead(t, record)
	require.NoError(t, st.ValidateXRemovalRecordContext(t.Context(), record, head), "pending scratch cleanup may be replayed only after its complete current authority is matched")
	for _, tc := range []struct {
		name string
		edit func(*store.XRemovalRecord)
	}{
		{"missing source", func(r *store.XRemovalRecord) { r.Sources = []store.XRemovalRecordSource{} }},
		{"recreated same row", func(r *store.XRemovalRecord) { r.Sources[0].InstanceID = strings.Repeat("b", 64) }},
		{"wrong numeric owner", func(r *store.XRemovalRecord) { r.Sources[0].OwnerID = "2001" }},
		{"wrong source row", func(r *store.XRemovalRecord) { r.Sources[0].SourceID++ }},
		{"missing known alias", func(r *store.XRemovalRecord) { r.Sources[0].RemovedIDs = []string{"100"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := record
			changed.Sources = slices.Clone(record.Sources)
			tc.edit(&changed)
			// This independently valid digest is not enough to replace the
			// restored archive's identity or forget its known suppressions.
			require.Error(t, st.ValidateXRemovalRecordContext(t.Context(), changed, xRestoreRecordHead(t, changed)))
		})
	}
}

func xRestoreRecordHead(t *testing.T, record store.XRemovalRecord) string {
	t.Helper()
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func TestXRemovalCaptureUsesOneExecutionSessionAndRefusesActiveSources(t *testing.T) {
	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(2)
	for _, owner := range []string{"1001", "2001", "3001"} {
		source, err := st.GetOrCreateSource("x", owner)
		require.NoError(t, err)
		_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: owner, InitializeInstance: true})
		require.NoError(t, err)
	}
	// This bounds an observable lock/snapshot transition, not runner throughput.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	record, head, err := st.CaptureXRemovalRecordContext(ctx)
	require.NoError(t, err)
	require.Len(t, record.Sources, 3)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), record.Sources[1].SourceID)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, execution.Release()) })
	_, _, err = st.CaptureXRemovalRecordContext(ctx)
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	require.NoError(t, execution.Release())
	again, againHead, err := st.CaptureXRemovalRecordContext(ctx)
	require.NoError(t, err)
	assert.Equal(t, record, again)
	assert.Equal(t, head, againHead)
}

func TestXRestoreTargetHeadTracksLatestChain(t *testing.T) {
	for _, missingLatest := range []bool{false, true} {
		name := "pre-lookup uncertainty"
		if missingLatest {
			name = "new edit not yet fetched"
		}
		t.Run(name, func(t *testing.T) {
			st, source := xPostFixture(t)
			seedXRestoreInstance(t, st, source, strings.Repeat("a", 64))
			input := xPostInput(source, "102", "100", "reviewed current text", "100", "102")
			_, err := st.PersistXPostContext(t.Context(), input)
			require.NoError(t, err)
			before, _, err := st.ExportXRemovalRecordContext(t.Context())
			require.NoError(t, err)
			if missingLatest {
				input.EditHistory = []string{"100", "102", "103"}
				input.FetchedAt = input.FetchedAt.Add(time.Hour)
				_, err = st.PersistXPostContext(t.Context(), input)
				require.NoError(t, err)
			} else {
				require.NoError(t, st.MarkXPostsUnverifiedContext(t.Context(), source, []string{"102"}))
			}
			target, err := st.ResolveXPostContext(t.Context(), source, "100")
			require.NoError(t, err)
			require.Equal(t, "unverified", target.Availability)
			current, head, err := st.ExportXRemovalRecordContext(t.Context())
			require.NoError(t, err)
			require.Len(t, current.Sources, 1)
			assert.Equal(t, before.Sources[0].FinancialHead, current.Sources[0].FinancialHead)
			assert.NotEqual(t, before.Sources[0].TargetHead, current.Sources[0].TargetHead)
			require.NoError(t, st.ValidateXRemovalRecordContext(t.Context(), current, head))
			assert.ErrorContains(t, st.ValidateXRemovalRecordContext(t.Context(), before, xRestoreRecordHead(t, before)), "reply-target verification")

			// A verified latest version clears the fence even though historical
			// placeholders retain their own unverified marker.
			if missingLatest {
				input.PostID = "103"
			}
			input.FetchedAt = input.FetchedAt.Add(2 * time.Hour)
			input.Author.FetchedAt = input.FetchedAt
			_, err = st.PersistXPostContext(t.Context(), input)
			require.NoError(t, err)
			verified, _, err := st.ExportXRemovalRecordContext(t.Context())
			require.NoError(t, err)
			assert.Equal(t, before.Sources[0].TargetHead, verified.Sources[0].TargetHead)
			require.NoError(t, st.ValidateXRemovalRecordContext(t.Context(), before, xRestoreRecordHead(t, before)))
		})
	}
}
