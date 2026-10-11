package query_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestBoundedMessageDetails covers each place GetMessage and GetMessageRaw
// apply the remote client byte limit, on whichever backend the store uses.
func TestBoundedMessageDetails(t *testing.T) {
	f := storetest.New(t)
	engine := originalEngine(f)
	withRaw := func(t *testing.T, raw string) int64 {
		t.Helper()
		id := f.NewMessage().Create(t, f.Store)
		require.NoError(t, f.Store.UpsertMessageRaw(id, []byte(raw)))
		return id
	}
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T) int64
		raw      bool
		limit    int64
		wantErr  error
		wantBody string
	}{
		{
			name: "stored body counts UTF-8 bytes",
			setup: func(t *testing.T) int64 {
				t.Helper()
				id := f.NewMessage().Create(t, f.Store)
				require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: strings.Repeat("\u00e9", 17), Valid: true}, sql.NullString{}))
				return id
			},
			limit:   32,
			wantErr: query.ErrOriginalMessageTooLarge,
		},
		{
			name: "raw MIME inflates past the limit",
			setup: func(t *testing.T) int64 {
				t.Helper()
				return withRaw(t, "Subject: Fixture\r\n\r\n"+strings.Repeat("x", 100000))
			},
			raw:     true,
			limit:   32,
			wantErr: query.ErrOriginalMessageTooLarge,
		},
		{
			name: "MIME body fallback keeps the raw limit",
			setup: func(t *testing.T) int64 {
				t.Helper()
				return withRaw(t, "Subject: Fixture\r\n\r\n"+strings.Repeat("x", 100000))
			},
			limit:   32,
			wantErr: query.ErrOriginalMessageTooLarge,
		},
		{
			name: "MIME body fallback rejects charset expansion",
			setup: func(t *testing.T) int64 {
				t.Helper()
				return withRaw(t, "From: sender@example.com\r\nSubject: Fixture\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\n"+strings.Repeat("\xe9", 600))
			},
			limit:   1024,
			wantErr: query.ErrOriginalMessageTooLarge,
		},
		{
			name: "MIME body fallback still serves dedup losers",
			setup: func(t *testing.T) int64 {
				t.Helper()
				id := withRaw(t, "From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Fixture\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\nFallback body.\r\n")
				_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
				require.NoError(t, err)
				return id
			},
			limit:    1024,
			wantBody: "Fallback body.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			id := tc.setup(t)
			ctx := query.WithMessageByteLimit(context.Background(), tc.limit)
			if tc.raw {
				_, err := engine.GetMessageRaw(ctx, id)
				require.ErrorIs(err, tc.wantErr)
				return
			}
			detail, err := engine.GetMessage(ctx, id)
			if tc.wantErr != nil {
				require.ErrorIs(err, tc.wantErr)
				return
			}
			require.NoError(err)
			assert.Equal(t, tc.wantBody, strings.TrimSpace(detail.BodyText))
		})
	}
}

func TestBoundedAllThreadMembershipNeverTruncates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	engine := originalEngine(f)
	first := f.NewMessage().Create(t, f.Store)
	for i := 1; i < query.ThreadMaxLimit; i++ {
		f.NewMessage().Create(t, f.Store)
	}
	thread := query.ThreadQuery{ID: first, All: true, MaxMembers: query.ThreadMaxLimit}
	page, err := engine.ListThread(context.Background(), thread)
	require.NoError(err)
	assert.Len(page.Messages, query.ThreadMaxLimit)

	f.NewMessage().Create(t, f.Store)
	_, err = engine.ListThread(context.Background(), thread)
	require.ErrorIs(err, query.ErrThreadTooLarge)
}
