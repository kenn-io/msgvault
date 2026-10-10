package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRecordingReferenceTeamsPointer(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("recording")
	refs := []store.AttachmentRef{{SourceAttachmentID: "teams:recording:abc", StoragePath: "https://teams.example.test/play?token=one"}}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET content_changed_at=? WHERE id=?`), "2000-01-01 00:00:00.000", id)
	require.NoError(err)
	require.NoError(f.Store.ReplaceMessageLinkAttachments(id, refs))
	var before, after string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT content_changed_at FROM messages WHERE id=?`), id).Scan(&before))
	assert.NotContains(before, "2000-01-01")
	require.NoError(f.Store.ReplaceMessageLinkAttachments(id, refs))
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT content_changed_at FROM messages WHERE id=?`), id).Scan(&after))
	assert.Equal(before, after)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET content_changed_at=? WHERE id=?`), "2000-01-01 00:00:00.000", id)
	require.NoError(err)
	refs[0].StoragePath = "https://teams.example.test/play?token=two"
	require.NoError(f.Store.ReplaceMessageLinkAttachments(id, refs))
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT content_changed_at FROM messages WHERE id=?`), id).Scan(&after))
	assert.NotContains(after, "2000-01-01")
}

func TestRecordingReferenceState(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("recording")
	input := store.RecordingReferenceInput{RouteKey: "route", RefSHA256: "hash", OccurrenceJSON: `{"ref":"ref","revision":"revision"}`}
	require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, []store.RecordingReferenceInput{input}))
	claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
	require.NoError(err)
	require.Len(claims, 1)
	first := claims[0]
	now := time.Now().UTC()
	ok, err := f.Store.FinishRecordingReference(t.Context(), first, store.RecordingReferenceResult{State: "uncertain", LastSendAt: &now, NextActionAt: now}, true, []store.RecordingReferenceInput{input})
	require.NoError(err)
	assert.True(ok)
	require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, false, nil))
	claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", now.Add(time.Hour), 20)
	require.NoError(err)
	require.Len(claims, 1)
	assert.Equal("uncertain", claims[0].State)
	ok, err = f.Store.FinishRecordingReference(t.Context(), first, store.RecordingReferenceResult{State: "withdrawn", NextActionAt: now}, false, nil)
	require.NoError(err)
	assert.True(ok)
	input.RefSHA256 = "new-hash"
	require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, []store.RecordingReferenceInput{input}))
	claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", now.Add(time.Hour), 20)
	require.NoError(err)
	require.Len(claims, 1)
	assert.NotEqual(first.OperationID, claims[0].OperationID)
	ok, err = f.Store.FinishRecordingReference(t.Context(), first, store.RecordingReferenceResult{State: "retained", NextActionAt: now}, true, []store.RecordingReferenceInput{input})
	require.NoError(err)
	assert.False(ok)
	ok, err = f.Store.MarkRecordingReferenceSending(t.Context(), first, now)
	require.NoError(err)
	assert.False(ok)
	before, err := f.Store.LoadRecordingReferenceCursor(t.Context(), "destination")
	require.NoError(err)
	after := store.RecordingReferenceCursor{At: now, AfterID: id, AfterRow: true, Policy: "policy"}
	ok, err = f.Store.AdvanceRecordingReferenceCursor(t.Context(), "destination", before, after)
	require.NoError(err)
	assert.True(ok)
	ok, err = f.Store.AdvanceRecordingReferenceCursor(t.Context(), "destination", before, after)
	require.NoError(err)
	assert.False(ok)
}

func TestRecordingReferenceOccurrenceCorrection(t *testing.T) {
	for _, state := range []string{"retained", "withdrawn", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("recording")
			input := store.RecordingReferenceInput{RouteKey: "route", RefSHA256: "hash", OccurrenceJSON: `{"ref":"original"}`}
			require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, []store.RecordingReferenceInput{input}))
			claims, err := f.Store.ClaimRecordingReferences(t.Context(), "destination", time.Now().Add(time.Hour), 20)
			require.NoError(err)
			require.Len(claims, 1)
			first := claims[0]
			now := time.Now().UTC().Truncate(time.Millisecond)
			result := store.RecordingReferenceResult{State: state, ErrorCode: "receipt_server_error", SourceID: "source", OccurrenceID: "occurrence", Outcome: "access_required", CoverageState: "unprocessed", LastSendAt: &now, RetryCount: 2, NextActionAt: now}
			ok, err := f.Store.FinishRecordingReference(t.Context(), first, result, state != "withdrawn", []store.RecordingReferenceInput{input})
			require.NoError(err)
			require.True(ok)
			input.OccurrenceJSON = `{"ref":"corrected"}`
			require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, []store.RecordingReferenceInput{input}))
			if state == "uncertain" {
				claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", now.Add(time.Hour), 20)
				require.NoError(err)
				require.Len(claims, 1)
				assert.Equal(first.OperationID, claims[0].OperationID)
				assert.Equal(first.OccurrenceJSON, claims[0].OccurrenceJSON)
				assert.Equal("uncertain", claims[0].State)
				assert.Equal(&now, claims[0].LastSendAt)
				assert.Equal(2, claims[0].RetryCount)
				assert.Equal(result.ErrorCode, claims[0].ErrorCode)
				result.State = "retained"
				ok, err = f.Store.FinishRecordingReference(t.Context(), first, result, true, []store.RecordingReferenceInput{input})
				require.NoError(err)
				require.True(ok)
				require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, []store.RecordingReferenceInput{input}))
			}
			claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", now.Add(time.Hour), 20)
			require.NoError(err)
			require.Len(claims, 1)
			corrected := claims[0]
			assert.NotEqual(first.OperationID, corrected.OperationID)
			assert.Equal(input.OccurrenceJSON, corrected.OccurrenceJSON)
			assert.Equal(first.RefSHA256, corrected.RefSHA256)
			assert.Equal("pending", corrected.State)
			assert.Nil(corrected.LastSendAt)
			assert.Zero(corrected.RetryCount)
			assert.Empty(corrected.ErrorCode)
			var source, occurrence, outcome, coverage string
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT source_id,occurrence_id,outcome,coverage_state FROM recording_references WHERE message_id=?`), id).Scan(&source, &occurrence, &outcome, &coverage))
			assert.Empty(source)
			assert.Empty(occurrence)
			assert.Empty(outcome)
			assert.Empty(coverage)
			ok, err = f.Store.FinishRecordingReference(t.Context(), first, result, true, []store.RecordingReferenceInput{first.RecordingReferenceInput})
			require.NoError(err)
			assert.False(ok)
			require.NoError(f.Store.ReconcileRecordingReferences(t.Context(), "destination", id, true, []store.RecordingReferenceInput{input}))
			claims, err = f.Store.ClaimRecordingReferences(t.Context(), "destination", now.Add(time.Hour), 20)
			require.NoError(err)
			require.Len(claims, 1)
			assert.Equal(corrected.OperationID, claims[0].OperationID)
		})
	}
}
