package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestFilesSearchRetainsCachedAttachmentAfterProviderResync(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newExploreIdentityAPIFixture(t)
	// The committed DuckDB fixture contains attachment 11 on message 1.
	// Give that SQLite row the same stable provider identity used on resync.
	_, err := f.store.DB().Exec(`UPDATE attachments SET
		source_attachment_id = 'discord:report', source_part_key = 'discord:report',
		storage_path = 'https://files.example/report.txt'
		WHERE id = 11`)
	requirements.NoError(err)
	refs, err := f.store.MessageDiscordAttachments(1)
	requirements.NoError(err)
	requirements.Contains(refs, "discord:report")

	first := postExploreJSON(t, f.server, "/api/v1/files/search", `{"predicate":{}}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var before FileSearchHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &before))
	requirements.Len(before.Files, 2)
	requirements.Equal(int64(11), before.Files[1].ID)

	requirements.NoError(f.store.ReplaceMessageDiscordAttachments(1, []store.AttachmentRef{refs["discord:report"]}))
	resynced := postExploreJSON(t, f.server, "/api/v1/files/search", `{"predicate":{}}`)
	requirements.Equal(http.StatusOK, resynced.Code, resynced.Body.String())
	var after FileSearchHTTPResponse
	requirements.NoError(json.Unmarshal(resynced.Body.Bytes(), &after))
	assertions.Equal(before.Files, after.Files)
	assertions.Equal(before.CacheRevision, after.CacheRevision)

	// A real removal still rejects the stale analytical row; preserving row
	// identity must not serve metadata for an attachment that no longer exists.
	requirements.NoError(f.store.ReplaceMessageDiscordAttachments(1, nil))
	removed := postExploreJSON(t, f.server, "/api/v1/files/search", `{"predicate":{}}`)
	assertions.Equal(http.StatusConflict, removed.Code)
	var failure ErrorResponse
	requirements.NoError(json.Unmarshal(removed.Body.Bytes(), &failure))
	assertions.Equal("file_metadata_changed", failure.Error)
}
