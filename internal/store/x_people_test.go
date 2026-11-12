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
	_testRequire := require.New(t)

	st := storetest.New(t).Store
	discord, err := st.GetOrCreateSource("discord", "9000")
	_testRequire.NoError(err)
	bad, err := st.GetOrCreateSource("x", "example_handle")
	_testRequire.NoError(err)
	for _, sourceID := range []int64{0, 999999, discord.ID, bad.ID} {
		_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: sourceID, UserID: "1001", Username: "example", FetchedAt: time.Now()})
		_testRequire.Error(err)
	}
	var count int
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='x_user_id'`).Scan(&count))
	assert.Zero(t, count)
}

func TestXAuthorStoreRejectsInvalidNormalizedInput(t *testing.T) {
	_testRequire := require.New(t)

	st := storetest.New(t).Store
	source, err := st.GetOrCreateSource("x", "9000")
	_testRequire.NoError(err)
	for _, id := range []string{"", "0", "01", "-1", "１２", strings.Repeat("9", 129)} {
		_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source.ID, UserID: id, FetchedAt: time.Now()})
		_testRequire.Error(err)
	}
	_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source.ID, UserID: "1001"})
	_testRequire.Error(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.RecordXAuthorContext(ctx, store.XAuthorInput{SourceID: source.ID, UserID: "1001", FetchedAt: time.Now()})
	_testRequire.ErrorIs(err, context.Canceled)
	pid, err := st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source.ID, UserID: strings.Repeat("9", 128), FetchedAt: time.Now()})
	_testRequire.NoError(err)
	assert.Positive(t, pid)
}

func TestXAuthorStoreRejectsCrossSourceScope(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := storetest.New(t).Store
	first, err := st.GetOrCreateSource("x", "9000")
	_testRequire.NoError(err)
	other, err := st.GetOrCreateSource("x", "9001")
	_testRequire.NoError(err)
	run, err := st.StartSync(first.ID, "full")
	_testRequire.NoError(err)
	scoped := st.ScopedToSync(first.ID, run)
	_, err = scoped.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: other.ID, UserID: "1001", FetchedAt: time.Now()})
	_testRequire.Error(err)
	var count int
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='x_user_id'`).Scan(&count))
	_testAssert.Zero(count)
	_testRequire.NoError(st.FailSync(run, "synthetic scope test complete"))
}
