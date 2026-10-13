package msmail

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMicrosoftInboxDeltaAndLookupSelectCompleteMetadata(t *testing.T) {
	for _, missing := range []string{"", "categories", "isRead", "parentFolderId", "@odata.etag"} {
		t.Run(missing, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, field := range []string{"id", "categories", "isRead", "parentFolderId"} {
					assertions.Contains(strings.Split(r.URL.Query().Get("$select"), ","), field)
				}
				message := map[string]any{"id": "immutable-1", "categories": []string{}, "isRead": false, "parentFolderId": "inbox-id", "@odata.etag": "version"}
				delete(message, missing)
				if strings.HasSuffix(r.URL.Path, "/delta") {
					assertions.NoError(json.MarshalWrite(w, map[string]any{"value": []any{message}, "@odata.deltaLink": "/next"}))
				} else {
					assertions.NoError(json.MarshalWrite(w, message))
				}
			}))
			defer srv.Close()
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
			page, err := c.DeltaPage(t.Context(), DeltaStartURL("inbox-id"))
			requirements.NoError(err)
			requirements.Len(page.Value, 1)
			info, err := c.LookupMessage(t.Context(), "immutable-1")
			requirements.NoError(err)
			assertions.Equal(page.Value[0].IsRead, info.IsRead)
			assertions.Equal(page.Value[0].ParentFolderID, info.ParentFolderID)
			assertions.Equal(page.Value[0].ETag, info.ETag)
			if missing == "isRead" {
				assertions.Nil(info.IsRead)
			} else {
				requirements.NotNil(info.IsRead)
				assertions.False(*info.IsRead)
			}
			if missing == "categories" {
				assertions.Nil(info.Categories)
			} else {
				requirements.NotNil(info.Categories)
				assertions.Empty(*info.Categories)
			}
		})
	}
}

func TestMicrosoftInboxLookupRejectsDifferentImmutableIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"id": "different-id", "categories": []string{"Foreign"}, "isRead": false, "parentFolderId": "archive", "@odata.etag": "foreign-version"}))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
	_, err := c.LookupMessage(t.Context(), "immutable-1")
	require.Error(t, err)
}

func TestMicrosoftInboxCursorUpgradeFromCategoriesV1(t *testing.T) {
	for _, scenario := range []string{"completed", "interrupted", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newFakeGraph(t)
			f.categories = map[string][]string{"m1": {}, "m2": {"Next"}}
			f.put("m1", "inbox")
			f.put("m2", "inbox")
			st := testutil.NewTestStore(t)
			initial, err := f.sync(t, st)
			requirements.NoError(err)
			previous, err := st.GetLastSuccessfulSync(initial.SourceID)
			requirements.NoError(err)
			var cursors map[string]string
			requirements.NoError(json.Unmarshal([]byte(previous.CursorAfter.String), &cursors))
			for folder, cursor := range cursors {
				cursors[folder] = "categories-v1:" + strings.TrimPrefix(strings.TrimPrefix(cursor, "categories-v1:"), "inbox-state-v2:")
			}
			encoded, err := json.Marshal(cursors)
			requirements.NoError(err)
			syncID, err := st.StartSync(initial.SourceID, SourceType)
			requirements.NoError(err)
			requirements.NoError(st.ScopedToSync(initial.SourceID, syncID).CompleteSyncAndPreserveSourceCursorContext(t.Context(), syncID, initial.SourceID, string(encoded)))
			f.walkStarts.Store(0)
			f.mimeCalls.Store(0)
			if scenario == "interrupted" {
				f.pageSize = 1
				f.stopAt = 1
			}
			if scenario == "expired" {
				f.expired["inbox"] = true
			}
			_, err = f.sync(t, st)
			if scenario == "interrupted" {
				requirements.Error(err)
				_, err = f.sync(t, st)
			}
			requirements.NoError(err)
			assertions.GreaterOrEqual(f.walkStarts.Load(), int32(2), "opaque old cursors cannot gain the new metadata selection")
			assertions.Zero(f.mimeCalls.Load(), "upgrading authoritative metadata does not redownload unchanged bodies")
			latest, err := st.GetLastSuccessfulSync(initial.SourceID)
			requirements.NoError(err)
			requirements.NoError(json.Unmarshal([]byte(latest.CursorAfter.String), &cursors))
			for _, cursor := range cursors {
				assertions.True(strings.HasPrefix(cursor, "inbox-state-v2:"))
			}
			f.walkStarts.Store(0)
			_, err = f.sync(t, st)
			requirements.NoError(err)
			assertions.Zero(f.walkStarts.Load())
		})
	}
}
