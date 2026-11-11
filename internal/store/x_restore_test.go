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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	before, err := st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Empty(before.InstanceID)
	first, err := st.UpdateXControlContext(t.Context(), source.ID, before.Revision, store.XControlPatch{OwnerID: "1001", InitializeInstance: true})
	_testRequire.NoError(err)
	_testAssert.Len(first.InstanceID, 64)
	decoded, err := hex.DecodeString(first.InstanceID)
	_testRequire.NoError(err)
	_testAssert.Len(decoded, 32)
	reconnected, err := st.UpdateXControlContext(t.Context(), source.ID, first.Revision, store.XControlPatch{OwnerID: "1001", InitializeInstance: true})
	_testRequire.NoError(err)
	_testAssert.Equal(first.InstanceID, reconnected.InstanceID)
	secondSource, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	second, err := st.UpdateXControlContext(t.Context(), secondSource.ID, 0, store.XControlPatch{OwnerID: "2001", InitializeInstance: true})
	_testRequire.NoError(err)
	_testAssert.NotEqual(first.InstanceID, second.InstanceID)
	_testAssert.Equal(before.OwnerID, reconnected.OwnerID)
	_testAssert.Equal(before.Generation, reconnected.Generation)
	_testAssert.Equal(before.Reservations, reconnected.Reservations)
}

func TestXRemovalRecordCapturesCompleteBoundedIdentitySet(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	instance := strings.Repeat("a", 64)
	seedXRestoreInstance(t, st, source, instance)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "synthetic aurora payload", "100", "102"))
	_testRequire.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,availability='removed',cleanup_pending=0 WHERE source_id=?`), source)
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	seedXRestoreInstance(t, st, other.ID, strings.Repeat("b", 64))
	_, err = st.GetOrCreateSource("gmail", "1001")
	_testRequire.NoError(err)
	uid, err := st.ArchiveUIDContext(t.Context())
	_testRequire.NoError(err)
	record, head, err := st.ExportXRemovalRecordContext(t.Context())
	_testRequire.NoError(err)
	_testAssert.Equal(4, record.Version)
	_testAssert.Equal(uid, record.ArchiveUID)
	_testRequire.Len(record.Sources, 2)
	_testAssert.Equal(source, record.Sources[0].SourceID)
	_testAssert.Equal("1001", record.Sources[0].OwnerID)
	_testAssert.Equal(instance, record.Sources[0].InstanceID)
	_testAssert.Equal([]string{"100", "102"}, record.Sources[0].RemovedIDs)
	_testAssert.Len(record.Sources[0].FinancialHead, 64)
	_testAssert.Equal(record.Sources[0].FinancialHead, record.Sources[1].FinancialHead, "retention metadata does not change empty financial authority")
	_testAssert.Equal(other.ID, record.Sources[1].SourceID)
	_testAssert.Empty(record.Sources[1].RemovedIDs)
	encoded, err := json.Marshal(record)
	_testRequire.NoError(err)
	digest := sha256.Sum256(encoded)
	_testAssert.Equal(hex.EncodeToString(digest[:]), head)
	_testRequire.NoError(store.ValidateXRemovalRecord(record, head, uid))
	for _, content := range []string{"aurora", "example_author", "Example Author", "access_token", "username", "text"} {
		_testAssert.NotContains(string(encoded), content)
	}
}

func TestXRemovalRecordRejectsPendingOrUnboundSource(t *testing.T) {
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	_, _, err := st.ExportXRemovalRecordContext(t.Context())
	require.Error(t, err, "a source without a verified immutable incarnation cannot authorize old snapshots")
	seedXRestoreInstance(t, st, source, strings.Repeat("a", 64))
	_, err = st.PersistXPostContext(t.Context(), xPostInput(source, "100", "100", "synthetic pending payload"))
	_testRequire.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,availability='removed',cleanup_pending=1 WHERE source_id=?`), source)
	_testRequire.NoError(err)
	_, _, err = st.ExportXRemovalRecordContext(t.Context())
	_testRequire.Error(err, "a pending operation cannot publish complete removal authority")
}

func TestXRemovalRecordRequiresIndependentCompleteHead(t *testing.T) {
	record := store.XRemovalRecord{Version: 4, ArchiveUID: strings.Repeat("c", 64), ProtectedAuthors: []store.XRemovalRecordAuthor{}, Sources: []store.XRemovalRecordSource{{SourceID: 42, OwnerID: "1001", InstanceID: strings.Repeat("a", 64), FinancialHead: strings.Repeat("e", 64), TargetHead: strings.Repeat("f", 64), RemovedIDs: []string{"100", "102"}}}}
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	head := hex.EncodeToString(digest[:])
	for _, tc := range []struct {
		name       string
		edit       func(*store.XRemovalRecord)
		structural bool
	}{
		{"shortened", func(r *store.XRemovalRecord) { r.Sources[0].RemovedIDs = []string{"100"} }, false},
		{"missing source", func(r *store.XRemovalRecord) { r.Sources = nil }, false},
		{"recreated source", func(r *store.XRemovalRecord) { r.Sources[0].InstanceID = strings.Repeat("b", 64) }, false},
		{"wrong owner", func(r *store.XRemovalRecord) { r.Sources[0].OwnerID = "2001" }, false},
		{"wrong source", func(r *store.XRemovalRecord) { r.Sources[0].SourceID = 43 }, false},
		{"duplicate ID", func(r *store.XRemovalRecord) { r.Sources[0].RemovedIDs = []string{"100", "100", "102"} }, true},
		{"noncanonical ID", func(r *store.XRemovalRecord) { r.Sources[0].RemovedIDs = []string{"0100", "102"} }, true},
		{"duplicate source", func(r *store.XRemovalRecord) { r.Sources = append(r.Sources, r.Sources[0]) }, true},
		{"invalid protected author", func(r *store.XRemovalRecord) {
			r.ProtectedAuthors = []store.XRemovalRecordAuthor{{UserID: "0100", ProtectedAt: time.Now()}}
		}, true},
		{"missing protection time", func(r *store.XRemovalRecord) { r.ProtectedAuthors = []store.XRemovalRecordAuthor{{UserID: "1001"}} }, true},
		{"duplicate protected author", func(r *store.XRemovalRecord) {
			a := store.XRemovalRecordAuthor{UserID: "1001", ProtectedAt: time.Now()}
			r.ProtectedAuthors = []store.XRemovalRecordAuthor{a, a}
		}, true},
		{"old version", func(r *store.XRemovalRecord) { r.Version-- }, true},
		{"future version", func(r *store.XRemovalRecord) { r.Version++ }, true},
		{"missing financial head", func(r *store.XRemovalRecord) { r.Sources[0].FinancialHead = "" }, true},
		{"missing target head", func(r *store.XRemovalRecord) { r.Sources[0].TargetHead = "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := record
			changed.Sources = slices.Clone(record.Sources)
			tc.edit(&changed)
			currentHead := head
			if tc.structural {
				currentHead = xRemovalRecordTestHead(t, changed)
			}
			err := store.ValidateXRemovalRecord(changed, currentHead, record.ArchiveUID)
			if tc.structural {
				require.EqualError(t, err, "invalid or incomplete X removal record")
			} else {
				require.EqualError(t, err, "x removal record does not match the independently trusted current head")
			}
		})
	}
	t.Run("missing current head", func(t *testing.T) { require.Error(t, store.ValidateXRemovalRecord(record, "", record.ArchiveUID)) })
	t.Run("wrong archive", func(t *testing.T) {
		require.Error(t, store.ValidateXRemovalRecord(record, head, strings.Repeat("d", 64)))
	})
}

func xRemovalRecordTestHead(t *testing.T, record store.XRemovalRecord) string {
	t.Helper()
	require := require.New(t)
	for i := range record.Sources {
		record.Sources[i].RemovedIDs = slices.Clone(record.Sources[i].RemovedIDs)
		slices.SortFunc(record.Sources[i].RemovedIDs, func(a, b string) int {
			if len(a) < len(b) {
				return -1
			}
			if len(a) > len(b) {
				return 1
			}
			return strings.Compare(a, b)
		})
	}
	slices.SortFunc(record.Sources, func(a, b store.XRemovalRecordSource) int {
		if a.SourceID < b.SourceID {
			return -1
		}
		if a.SourceID > b.SourceID {
			return 1
		}
		return 0
	})
	encoded, err := json.Marshal(record)
	require.NoError(err)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func seedXRestoreInstance(t *testing.T, st *store.Store, source int64, instance string) {
	t.Helper()
	state, err := st.LoadXControlContext(t.Context(), source)
	require.NoError(t, err)
	state.InstanceID = instance
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor=? WHERE id=?`), string(encoded), source)
	require.NoError(t, err)
}

func TestXRemovalRecordMatchesRestoredSourceAndKnownSuppressions(t *testing.T) {
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	instance := strings.Repeat("a", 64)
	seedXRestoreInstance(t, st, source, instance)
	_, err := st.PersistXPostContext(t.Context(), xPostInput(source, "102", "100", "synthetic aurora payload", "100", "102"))
	_testRequire.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE x_post_versions SET suppressed_at=CURRENT_TIMESTAMP,availability='removed',cleanup_pending=1 WHERE source_id=?`), source)
	_testRequire.NoError(err)
	uid, err := st.ArchiveUIDContext(t.Context())
	_testRequire.NoError(err)
	// Capture completed current authority, then restore the pending scratch
	// flag to prove validation allows the same local cleanup to resume.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE x_post_versions SET cleanup_pending=0 WHERE source_id=?`), source)
	_testRequire.NoError(err)
	record, _, err := st.ExportXRemovalRecordContext(t.Context())
	_testRequire.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE x_post_versions SET cleanup_pending=1 WHERE source_id=?`), source)
	_testRequire.NoError(err)
	assert.Equal(t, uid, record.ArchiveUID)
	head := xRestoreRecordHead(t, record)
	_testRequire.NoError(st.ValidateXRemovalRecordContext(t.Context(), record, head), "pending scratch cleanup may be replayed only after its complete current authority is matched")
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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(2)
	for _, owner := range []string{"1001", "2001", "3001"} {
		source, err := st.GetOrCreateSource("x", owner)
		_testRequire.NoError(err)
		_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: owner, InitializeInstance: true})
		_testRequire.NoError(err)
	}
	// This bounds an observable lock/snapshot transition, not runner throughput.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	record, head, err := st.CaptureXRemovalRecordContext(ctx)
	_testRequire.NoError(err)
	_testRequire.Len(record.Sources, 3)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), record.Sources[1].SourceID)
	_testRequire.NoError(err)
	t.Cleanup(func() { assert.NoError(t, execution.Release()) })
	_, _, err = st.CaptureXRemovalRecordContext(ctx)
	_testRequire.ErrorIs(err, store.ErrSyncAlreadyActive)
	_testRequire.NoError(execution.Release())
	again, againHead, err := st.CaptureXRemovalRecordContext(ctx)
	_testRequire.NoError(err)
	_testAssert.Equal(record, again)
	_testAssert.Equal(head, againHead)
}

func TestXRestoreTargetHeadTracksLatestChain(t *testing.T) {
	for _, missingLatest := range []bool{false, true} {
		name := "pre-lookup uncertainty"
		if missingLatest {
			name = "new edit not yet fetched"
		}
		t.Run(name, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st, source := xPostFixture(t)
			seedXRestoreInstance(t, st, source, strings.Repeat("a", 64))
			input := xPostInput(source, "102", "100", "reviewed current text", "100", "102")
			_, err := st.PersistXPostContext(t.Context(), input)
			_testRequire.NoError(err)
			before, _, err := st.ExportXRemovalRecordContext(t.Context())
			_testRequire.NoError(err)
			if missingLatest {
				input.EditHistory = []string{"100", "102", "103"}
				input.FetchedAt = input.FetchedAt.Add(time.Hour)
				_, err = st.PersistXPostContext(t.Context(), input)
				_testRequire.NoError(err)
			} else {
				_testRequire.NoError(st.MarkXPostsUnverifiedContext(t.Context(), source, []string{"102"}))
			}
			target, err := st.ResolveXPostContext(t.Context(), source, "100")
			_testRequire.NoError(err)
			_testRequire.Equal("unverified", target.Availability)
			current, head, err := st.ExportXRemovalRecordContext(t.Context())
			_testRequire.NoError(err)
			_testRequire.Len(current.Sources, 1)
			_testAssert.Equal(before.Sources[0].FinancialHead, current.Sources[0].FinancialHead)
			_testAssert.NotEqual(before.Sources[0].TargetHead, current.Sources[0].TargetHead)
			_testRequire.NoError(st.ValidateXRemovalRecordContext(t.Context(), current, head))
			_testRequire.ErrorContains(st.ValidateXRemovalRecordContext(t.Context(), before, xRestoreRecordHead(t, before)), "reply-target verification")

			// A verified latest version clears the fence even though historical
			// placeholders retain their own unverified marker.
			if missingLatest {
				input.PostID = "103"
			}
			input.FetchedAt = input.FetchedAt.Add(2 * time.Hour)
			input.Author.FetchedAt = input.FetchedAt
			_, err = st.PersistXPostContext(t.Context(), input)
			_testRequire.NoError(err)
			verified, _, err := st.ExportXRemovalRecordContext(t.Context())
			_testRequire.NoError(err)
			_testAssert.Equal(before.Sources[0].TargetHead, verified.Sources[0].TargetHead)
			_testRequire.NoError(st.ValidateXRemovalRecordContext(t.Context(), before, xRestoreRecordHead(t, before)))
		})
	}
}

func TestXRemovalRecordRetainsAuthorProtectionAfterSourceRemoval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := xPostFixture(t)
	seedXRestoreInstance(t, st, source, strings.Repeat("a", 64))
	before, head, err := st.ExportXRemovalRecordContext(t.Context())
	require.NoError(err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source, UserID: "2001", Protected: true, FetchedAt: at})
	require.NoError(err)
	require.ErrorContains(st.ValidateXRemovalRecordContext(t.Context(), before, head), "omits protection known to the snapshot")
	record, _, err := st.ExportXRemovalRecordContext(t.Context())
	require.NoError(err)
	require.Len(record.ProtectedAuthors, 1)
	assert.Equal("2001", record.ProtectedAuthors[0].UserID)
	assert.True(at.Equal(record.ProtectedAuthors[0].ProtectedAt))
	require.NoError(st.RemoveSource(source))
	after, _, err := st.ExportXRemovalRecordContext(t.Context())
	require.NoError(err)
	assert.Empty(after.Sources)
	assert.Equal(record.ProtectedAuthors, after.ProtectedAuthors)
}
