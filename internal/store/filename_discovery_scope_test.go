package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
	"time"
)

// Each family, source, account/date intersection and visibility edge has a
// literal population. No expected set is computed by the predicate under test.
func filenameIntersectionFixture(t *testing.T) (*storetest.Fixture, []int64, int64, time.Time, time.Time) {
	t.Helper()
	require := require.New(t)

	f := storetest.New(t)
	alternate, err := f.Store.GetOrCreateSource("imap", "shared@example.test")
	require.NoError(err)
	alternateConv, err := f.Store.EnsureConversation(alternate.ID, "alternate", "Alternate")
	require.NoError(err)
	after := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	mimes := []any{"image/png", "APPLICATION/PDF", "audio/mpeg", "video/mp4", "text/plain", "application/msword", "application/zip", "application/octet-stream", nil, "application/pdf", "application/pdf"}
	accounts := []any{"alpha@example.test", "beta@example.test", nil, "beta@example.test", "alpha@example.test", "alpha@example.test", nil, "beta@example.test", nil, "alpha@example.test", "alpha@example.test"}
	var ids []int64
	for i, mime := range mimes {
		source, conv := f.Source.ID, f.ConvID
		if i == 4 {
			source, conv = alternate.ID, alternateConv
		}
		var sent any = time.Date(2026, 2, i+1, 0, 0, 0, 0, time.UTC)
		if i == 5 {
			sent = nil
		}
		path := "inbound"
		if i == 1 {
			path = "sent"
		}
		var message, id int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at,account_address,account_path) VALUES(?,?,?,'email',?,?,?) RETURNING id`), source, conv, "scope-"+time.Date(2026, 2, i+1, 0, 0, 0, 0, time.UTC).Format("02"), sent, accounts[i], path).Scan(&message))
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,mime_type,storage_path) VALUES(?,'report.pdf',?,'') RETURNING id`), message, mime).Scan(&id))
		if i == 9 {
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), message)
			require.NoError(err)
		}
		if i == 10 {
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), message)
			require.NoError(err)
		}
		ids = append(ids, id)
	}
	return f, ids, alternate.ID, after, before
}

func TestFilenameSuggestionScopeIntersection(t *testing.T) {
	f, ids, alternateID, after, before := filenameIntersectionFixture(t)
	sourceID := f.Source.ID
	for _, tc := range []struct {
		name  string
		scope store.FilenameSuggestionScope
		want  []int
	}{
		{"all-visible", store.FilenameSuggestionScope{}, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 10}},
		{"source", store.FilenameSuggestionScope{SourceIDs: []int64{f.Source.ID, f.Source.ID}}, []int{0, 1, 2, 3, 5, 6, 7, 8, 10}},
		{"after-inclusive-before-exclusive", store.FilenameSuggestionScope{After: &after, Before: &before}, []int{1, 2, 3}},
		{"mime-and-date", store.FilenameSuggestionScope{After: &after, Before: &before, MIMEFamilies: []string{"pdf", "audio"}}, []int{1, 2}},
		{"alpha-or-unattributed-inbound", store.FilenameSuggestionScope{AccountScopes: []search.AccountScope{{Addresses: []string{"alpha@example.test"}, Unattributed: true, Inbound: true}}}, []int{0, 2, 4, 5, 6, 8, 10}},
		{"separate-account-scopes-intersect", store.FilenameSuggestionScope{AccountScopes: []search.AccountScope{{Addresses: []string{"alpha@example.test", "beta@example.test"}}, {Addresses: []string{"beta@example.test"}, Inbound: true}}}, []int{3, 7}},
		{"source-specific-account", store.FilenameSuggestionScope{AccountScopes: []search.AccountScope{{SourceID: &sourceID, Addresses: []string{"alpha@example.test"}}}}, []int{0, 5, 10}},
		{"every-constraint-mandatory", store.FilenameSuggestionScope{SourceIDs: []int64{alternateID}, After: &after, Before: &before, MIMEFamilies: []string{"image"}, AccountScopes: []search.AccountScope{{Addresses: []string{"alpha@example.test"}, Inbound: true}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := f.Store.FilenameSuggestionRowsContext(t.Context(), ids, tc.scope)
			require.NoError(t, err)
			var got, want []int64
			for _, row := range rows {
				got = append(got, row.File.ID)
				assert.Equal(t, "report.pdf", row.File.Filename)
			}
			for _, i := range tc.want {
				want = append(want, ids[i])
			}
			assert.ElementsMatch(t, want, got)
		})
	}
	assert := assert.New(t)
	require := require.New(t)
	for _, tc := range []struct {
		family string
		index  int
	}{{"image", 0}, {"pdf", 1}, {"audio", 2}, {"video", 3}, {"text", 4}, {"document", 5}, {"archive", 6}, {"other", 7}} {
		rows, err := f.Store.FilenameSuggestionRowsContext(t.Context(), []int64{ids[tc.index]}, store.FilenameSuggestionScope{MIMEFamilies: []string{tc.family}})
		require.NoError(err)
		require.Len(rows, 1)
		assert.Equal(tc.family, rows[0].MIMEFamily)
		if tc.index == 5 {
			assert.Nil(rows[0].OccurredAt)
		} else {
			assert.NotNil(rows[0].OccurredAt)
		}
	}
	// The metadata authority preserves duplicate occurrences and source deletion.
	visible, err := f.Store.GetFileMetadataBatch(t.Context(), ids)
	require.NoError(err)
	rows, err := f.Store.FilenameSuggestionRowsContext(t.Context(), ids, store.FilenameSuggestionScope{})
	require.NoError(err)
	require.Len(rows, len(visible))
	for _, row := range rows {
		assert.Equal(visible[row.File.ID], row.File)
	}
}

func TestFilenameSuggestionScopeDateBoundsCompareOffsetInstants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	laterMessage := f.CreateMessage("filename-offset-later")
	earlierMessage := f.CreateMessage("filename-offset-earlier")
	var laterAttachment, earlierAttachment int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path)
		VALUES(?,'later.pdf','') RETURNING id`), laterMessage).Scan(&laterAttachment))
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path)
		VALUES(?,'earlier.pdf','') RETURNING id`), earlierMessage).Scan(&earlierAttachment))
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), "2024-01-15 11:00:00-05:00", laterMessage)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=? WHERE id=?`), "2024-01-15 10:00:00-05:00", earlierMessage)
	require.NoError(err)
	bound := time.Date(2024, 1, 15, 15, 30, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		scope store.FilenameSuggestionScope
		want  int64
	}{
		{name: "after", scope: store.FilenameSuggestionScope{After: &bound}, want: laterAttachment},
		{name: "before", scope: store.FilenameSuggestionScope{Before: &bound}, want: earlierAttachment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := f.Store.FilenameSuggestionRowsContext(t.Context(), []int64{laterAttachment, earlierAttachment}, tc.scope)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(tc.want, rows[0].File.ID)
		})
	}
}
