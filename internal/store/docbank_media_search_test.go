package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocumentSearchVersionsAndOccurrences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("reader@example.com", "Reader", "example.com")
	var visible []int64
	for i, name := range []string{"old", "new", "undated", "deleted", "hidden", "other person", "alias"} {
		mid := f.CreateMessage(name)
		hash := fmt.Sprintf("%064x", i+1)
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: name + ".pdf", StoragePath: hash[:2] + "/" + hash, State: "stored"}))
		var date any
		if name != "undated" {
			date = time.Date(2026, 1, i+1, 0, 0, 0, 0, time.UTC)
		}
		_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=?,received_at=NULL,internal_date=NULL,sender_id=? WHERE id=?`), date, participant, mid)
		require.NoError(err)
		switch name {
		case "deleted":
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), time.Now().UTC(), mid)
		case "hidden":
			_, err = f.Store.MergeDuplicates(f.CreateMessage("survivor"), []int64{mid}, "hidden")
		case "other person":
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sender_id=NULL WHERE id=?`), mid)
		case "alias":
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET content_hash=NULL,source_attachment_id='slack:file' WHERE message_id=?`), mid)
		}
		require.NoError(err)
		require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 7, VersionID: name}, ""))
		if name == "old" || name == "new" || name == "undated" || name == "alias" {
			visible = append(visible, mid)
		}
	}
	scope := &personscope.Scope{ParticipantIDs: []int64{participant}, Directions: []personscope.Direction{personscope.FromPerson}}
	versions, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", scope, nil)
	require.NoError(err)
	var ids []string
	for _, version := range versions {
		ids = append(ids, version.VersionID)
	}
	assert.Equal([]string{"alias", "new", "old", "undated"}, ids)
	page, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", scope, &versions[1])
	require.NoError(err)
	require.Len(page, 2)
	assert.Equal("old", page[0].VersionID)
	assert.Equal("undated", page[1].VersionID)
	last, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", scope, &page[1])
	require.NoError(err)
	assert.Empty(last)
	occurrences, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", scope, ids)
	require.NoError(err)
	var mids []int64
	for _, o := range occurrences {
		mids = append(mids, o.MessageID)
		assert.Equal(o.VersionID+".pdf", o.Filename)
	}
	assert.Equal([]int64{visible[3], visible[1], visible[0], visible[2]}, mids)
	other, err := f.Store.DocumentSearchOccurrences(t.Context(), "other", scope, ids)
	require.NoError(err)
	assert.Empty(other)
	onlyNew, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", scope, []string{"new"})
	require.NoError(err)
	require.Len(onlyNew, 1)
	assert.Equal("new.pdf", onlyNew[0].Filename)
}

func TestDocumentSearchSharedVersionNewestOccurrence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	hash := fmt.Sprintf("%064x", 1)
	old, recent := f.CreateMessage("old"), f.CreateMessage("recent")
	for _, mid := range []int64{old, recent} {
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: "report.pdf", StoragePath: hash[:2] + "/" + hash, State: "stored"}))
	}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), old)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), recent)
	require.NoError(err)
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 7, VersionID: "shared"}, ""))
	occurrences, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", nil, []string{"shared"})
	require.NoError(err)
	require.Len(occurrences, 2)
	assert.Equal(recent, occurrences[0].MessageID)
	assert.Equal(old, occurrences[1].MessageID)
}

func TestDocumentSearchVersionCursorTies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	for i, version := range []string{"dated-a", "dated-b", "undated-a", "undated-b"} {
		mid := f.CreateMessage(version)
		hash := fmt.Sprintf("%064x", i+1)
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{ContentHash: hash, Filename: "report.pdf", StoragePath: hash[:2] + "/" + hash, State: "stored"}))
		if i < 2 {
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), mid)
			require.NoError(err)
		}
		require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 7, VersionID: version}, ""))
	}
	versions, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", nil, nil)
	require.NoError(err)
	require.Len(versions, 4)
	assert.Equal("dated-a", versions[0].VersionID)
	for i := range len(versions) - 1 {
		next, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", nil, &versions[i])
		require.NoError(err)
		assert.Equal(versions[i+1:], next)
	}
}
