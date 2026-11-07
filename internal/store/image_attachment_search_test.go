package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImageAttachmentSearchFencesAndRevalidatesVisibleOccurrences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("reader@example.com", "Reader", "example.com")
	mid := f.CreateMessage("synthetic image thread")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id=? WHERE id=?`), participant, mid)
	require.NoError(err)
	var formats []storetest.DocumentAttachment
	for i, mediaType := range []string{"image/jpeg", "image/png", "image/webp"} {
		formats = append(formats, storetest.DocumentAttachment{MIMEType: mediaType, VersionID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)})
	}
	formats = append(formats, storetest.DocumentAttachment{MIMEType: "audio/mpeg", VersionID: "00000000-0000-4000-8000-000000000004"}, storetest.DocumentAttachment{MIMEType: "video/mp4", VersionID: "00000000-0000-4000-8000-000000000005"})
	f.SeedDocumentAttachments(mid, formats)
	scope := &personscope.Scope{ParticipantIDs: []int64{participant}, Directions: []personscope.Direction{personscope.FromPerson}}
	ids, exceeded, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", scope)
	require.NoError(err)
	assert.False(exceeded)
	require.ElementsMatch([]string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"}, ids)
	occurrences, err := f.Store.DocumentSearchOccurrences(t.Context(), "attachments", scope, ids, 100)
	require.NoError(err)
	require.Len(occurrences, 3)
	assert.Equal(mid, occurrences[0].MessageID)
	other, _, err := f.Store.DocumentSearchVersions(t.Context(), "other-destination", scope)
	require.NoError(err)
	assert.Empty(other)
	otherScope := &personscope.Scope{ParticipantIDs: []int64{participant + 100}, Directions: []personscope.Direction{personscope.FromPerson}}
	other, _, err = f.Store.DocumentSearchVersions(t.Context(), "attachments", otherScope)
	require.NoError(err)
	assert.Empty(other)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), time.Now().UTC(), mid)
	require.NoError(err)
	occurrences, err = f.Store.DocumentSearchOccurrences(t.Context(), "attachments", scope, ids, 100)
	require.NoError(err)
	assert.Empty(occurrences)
}

func TestImageAttachmentSearchCountsImagesTowardScopeLimit(t *testing.T) {
	f := storetest.New(t)
	mid := f.CreateMessage("synthetic image collection")
	formats := make([]storetest.DocumentAttachment, 4097)
	for i := range formats {
		formats[i] = storetest.DocumentAttachment{MIMEType: "image/png", VersionID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)}
	}
	f.SeedDocumentAttachments(mid, formats)
	ids, exceeded, err := f.Store.DocumentSearchVersions(t.Context(), "attachments", nil)
	require.NoError(t, err)
	assert.True(t, exceeded)
	assert.Empty(t, ids)
}
