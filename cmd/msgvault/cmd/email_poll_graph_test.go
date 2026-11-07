package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRapidEmailGraphProfileMismatchPrecedesArchiveMutation(t *testing.T) {
	f := storetest.New(t)
	src, err := f.Store.GetOrCreateSource("msmail", "sample@example.test")
	require.NoError(t, err)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/me", r.URL.Path)
		assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"mail":"other@example.test"}`))
	}))
	defer server.Close()
	assert := assert.New(t)
	require := require.New(t)
	client := msmail.NewClient(server.URL, func(context.Context) (string, error) { return "fixture-token", nil }, 1000)
	dir := t.TempDir()
	ctx := testInvocationContext(t.Context(), &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}, invocationOptions{})
	err = runRapidGraphSource(ctx, src, f.Store, client, invocationFromContext(ctx))
	require.ErrorContains(err, "reauthorize")
	assert.EqualValues(1, calls.Load())
	for _, table := range []string{"account_identities", "sync_runs", "messages"} {
		var count int
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
		assert.Zero(count, "authenticated evidence precedes %s writes", table)
	}
}
