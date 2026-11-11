package x

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXArchivePreservesAllowanceForDueRefresh(t *testing.T) {
	for _, mode := range []string{"overdue", "incomplete", "preview", "zero cap"} {
		t.Run(mode, func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source := importTestSource(t, st)
			now := time.Now().UTC().Truncate(time.Second)
			fetched := now.Add(-48 * time.Hour)
			if mode == "incomplete" {
				fetched = now
			}
			_, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "100", AuthorID: "1001", ConversationID: "100", EditHistory: []string{"100"}, Text: "synthetic overdue content", CreatedAt: fetched, FetchedAt: fetched, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: fetched}})
			_testRequire.NoError(err)
			state, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			username := "example_owner"
			refresh := state.Refresh
			if mode == "incomplete" {
				refresh.SweepStartedAt = now.Add(-time.Minute)
			}
			_, err = st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Username: &username, Refresh: &refresh})
			_testRequire.NoError(err)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); wireImportPage(w, nil, "") }))
			t.Cleanup(server.Close)
			credentials := &archiveCredentials{importCredentials: &importCredentials{snapshot: CredentialSnapshot{OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}}
			policy := importTestPolicy(t)
			policy.MonthlyBudgetMicros = 1_500_000 // One archive page could consume all remaining allowance.
			imp := NewImporter(st, NewClient(server.URL, server.Client()), policy, credentials)
			imp.now = func() time.Time { return now }
			spendCap := int64(25_000_000)
			opts := ImportOptions{SourceID: source.ID, OwnerID: "1001", Archive: true, After: now.AddDate(-1, 0, 0), MaxSpendMicros: &spendCap}
			if mode == "preview" {
				opts.MaxSpendMicros = nil
			}
			if mode == "zero cap" {
				spendCap = 0
			}
			summary, err := imp.Import(t.Context(), opts)
			if mode == "preview" {
				_testRequire.NoError(err)
				_testAssert.True(summary.Archive.Preview)
			} else if mode != "zero cap" {
				_testRequire.ErrorContains(err, fmt.Sprintf("hint: run 'msgvault source x sync --source-id %d' without --archive", source.ID))
				_testAssert.Equal("refresh_required", summary.Code)
			}
			_testAssert.Zero(calls.Load(), "historic expansion cannot consume refresh headroom")
			if mode != "zero cap" {
				_testAssert.Zero(credentials.prepares.Load(), "no credential refresh before refresh-first refusal or preview")
			}
			current, err := st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			_testAssert.Empty(current.Reservations)
			_testAssert.Empty(current.SpendBuckets)
		})
	}
}
