//go:build fts5

package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestExploreFullTextHonorsDeletionScope drives the explore handler
// end-to-end with a real store resolving lexical candidates: the FTS index
// keeps entries for source-deleted messages, so a deletion:deleted full-text
// search must return them, an unrestricted search must cover them without
// declaring a narrowed scope, and deletion:active must exclude them.
// Before the fix, candidate resolution hard-coded the active-only scope, so
// deletion:deleted returned nothing and unrestricted searches silently
// omitted source-deleted matches.
func TestExploreFullTextHonorsDeletionScope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := storetest.New(t)
	liveID := f.NewMessage().
		WithSourceMessageID("m1").
		WithSubject("Older").
		WithSnippet("glacier match").
		Create(t, f.Store)
	requirements.NoError(f.Store.UpsertMessageBody(liveID,
		sql.NullString{String: "glacier body live", Valid: true},
		sql.NullString{}), "UpsertMessageBody live")
	deletedID := f.NewMessage().
		WithSourceMessageID("m2").
		WithSubject("Newest").
		WithSnippet("glacier beta").
		Create(t, f.Store)
	requirements.NoError(f.Store.UpsertMessageBody(deletedID,
		sql.NullString{String: "glacier body archived", Valid: true},
		sql.NullString{}), "UpsertMessageBody deleted")
	_, err := f.Store.BackfillFTS(nil)
	requirements.NoError(err, "BackfillFTS")
	requirements.NoError(f.Store.MarkMessageDeleted(f.Source.ID, "m2"),
		"MarkMessageDeleted")

	// The Parquet fixture below hard-codes message IDs 1 and 2; the fresh
	// store must have assigned the same IDs for candidates to line up.
	requirements.Equal(int64(1), liveID)
	requirements.Equal(int64(2), deletedID)

	engine, _ := newExploreDuckDBFixtureWithMessages(t,
		`(1::BIGINT, 1::BIGINT, 'm1', 101::BIGINT, 'Older', 'glacier match', TIMESTAMP '2026-07-18 10:00:00', 100::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2026, 7),
		(2::BIGINT, 1::BIGINT, 'm2', 102::BIGINT, 'Newest', 'glacier beta', TIMESTAMP '2026-07-18 11:00:00', 200::BIGINT, false, 0::INTEGER, TIMESTAMP '2026-07-19 00:00:00', NULL::BIGINT, 'email', 2026, 7)`,
		2)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  f.Store, Engine: engine, Logger: testLogger(),
	})

	deleted := postExploreJSON(t, srv, "/api/v1/explore", `{
		"query":"glacier","search_mode":"full_text",
		"filters":[{"dimension":"deletion","values":["deleted"]}]
	}`)
	requirements.Equal(http.StatusOK, deleted.Code, deleted.Body.String())
	var deletedBody ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(deleted.Body.Bytes(), &deletedBody))
	requirements.Len(deletedBody.Rows, 1,
		"deletion:deleted full text must return the source-deleted match")
	requirements.NotNil(deletedBody.Rows[0].AnchorMessageID)
	assertions.Equal(deletedID, *deletedBody.Rows[0].AnchorMessageID)

	unrestricted := postExploreJSON(t, srv, "/api/v1/explore",
		`{"query":"glacier","search_mode":"full_text"}`)
	requirements.Equal(http.StatusOK, unrestricted.Code, unrestricted.Body.String())
	var raw map[string]json.RawMessage
	requirements.NoError(json.Unmarshal(unrestricted.Body.Bytes(), &raw))
	assertions.NotContains(raw, "search_deletion_scope",
		"full text honors the unrestricted scope instead of declaring a narrowed one")
	var unrestrictedBody ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(unrestricted.Body.Bytes(), &unrestrictedBody))
	requirements.Len(unrestrictedBody.Rows, 2,
		"unrestricted full text must cover source-deleted messages")
	anchors := make([]int64, 0, len(unrestrictedBody.Rows))
	for _, row := range unrestrictedBody.Rows {
		requirements.NotNil(row.AnchorMessageID)
		anchors = append(anchors, *row.AnchorMessageID)
	}
	assertions.ElementsMatch([]int64{liveID, deletedID}, anchors)
	requirements.NotNil(unrestrictedBody.TotalCount)
	assertions.Equal(int64(2), *unrestrictedBody.TotalCount)

	active := postExploreJSON(t, srv, "/api/v1/explore", `{
		"query":"glacier","search_mode":"full_text",
		"filters":[{"dimension":"deletion","values":["active"]}]
	}`)
	requirements.Equal(http.StatusOK, active.Code, active.Body.String())
	var activeBody ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(active.Body.Bytes(), &activeBody))
	requirements.Len(activeBody.Rows, 1)
	requirements.NotNil(activeBody.Rows[0].AnchorMessageID)
	assertions.Equal(liveID, *activeBody.Rows[0].AnchorMessageID)
}
