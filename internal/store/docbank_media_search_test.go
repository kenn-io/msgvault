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
	versions, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", scope, 0)
	require.NoError(err)
	assert.Equal([]string{"alias", "new", "old", "undated"}, versions)
	page, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", scope, 2)
	require.NoError(err)
	assert.Equal([]string{"old", "undated"}, page)
	occurrences, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", scope, versions)
	require.NoError(err)
	var mids []int64
	for _, o := range occurrences {
		mids = append(mids, o.MessageID)
		assert.Equal(o.VersionID+".pdf", o.Filename)
	}
	assert.ElementsMatch(visible, mids)
	other, err := f.Store.DocumentSearchOccurrences(t.Context(), "other", scope, versions)
	require.NoError(err)
	assert.Empty(other)
	onlyNew, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", scope, []string{"new"})
	require.NoError(err)
	require.Len(onlyNew, 1)
	assert.Equal("new.pdf", onlyNew[0].Filename)
}
