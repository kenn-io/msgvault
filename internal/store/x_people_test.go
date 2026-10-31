package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestXAuthorStoreRequiresNumericXSource(t *testing.T) {
	st := storetest.New(t).Store
	discord, err := st.GetOrCreateSource("discord", "9000")
	require.NoError(t, err)
	bad, err := st.GetOrCreateSource("x", "example_handle")
	require.NoError(t, err)
	for _, sourceID := range []int64{0, 999999, discord.ID, bad.ID} {
		_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: sourceID, UserID: "1001", Username: "example", FetchedAt: time.Now()})
		require.Error(t, err)
	}
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='x_user_id'`).Scan(&count))
	assert.Zero(t, count)
}

func TestXAuthorStoreRejectsInvalidNormalizedInput(t *testing.T) {
	st := storetest.New(t).Store
	source, err := st.GetOrCreateSource("x", "9000")
	require.NoError(t, err)
	for _, id := range []string{"", "0", "01", "-1", "１２", strings.Repeat("9", 129)} {
		_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source.ID, UserID: id, FetchedAt: time.Now()})
		require.Error(t, err)
	}
	_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source.ID, UserID: "1001"})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.RecordXAuthorContext(ctx, store.XAuthorInput{SourceID: source.ID, UserID: "1001", FetchedAt: time.Now()})
	require.ErrorIs(t, err, context.Canceled)
	pid, err := st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source.ID, UserID: strings.Repeat("9", 128), FetchedAt: time.Now()})
	require.NoError(t, err)
	assert.Positive(t, pid)
}

func TestXAuthorStoreRejectsCrossSourceScope(t *testing.T) {
	st := storetest.New(t).Store
	first, err := st.GetOrCreateSource("x", "9000")
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("x", "9001")
	require.NoError(t, err)
	run, err := st.StartSync(first.ID, "full")
	require.NoError(t, err)
	scoped := st.ScopedToSync(first.ID, run)
	_, err = scoped.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: other.ID, UserID: "1001", FetchedAt: time.Now()})
	assert.Error(t, err)
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='x_user_id'`).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, st.FailSync(run, "synthetic scope test complete"))
}
