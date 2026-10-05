package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChatwootRegisteredInboxSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src := config.ChatwootSource{Identifier: "support", URL: "https://chatwoot.example.com", AccountID: 9, ExcludeInboxes: []int64{8}}
	for _, id := range []string{chatwoot.SourceIdentifier(src.URL, 9, 7), chatwoot.SourceIdentifier(src.URL, 9, 8), chatwoot.SourceIdentifier(src.URL, 10, 7)} {
		_, err := st.GetOrCreateSource(chatwoot.SourceType, id)
		require.NoError(err)
	}
	ids, err := resolveChatwootSyncInboxes(st, src, nil)
	require.NoError(err)
	assert.Equal([]int64{7}, ids)
	ids, err = resolveChatwootSyncInboxes(st, src, []int64{7, 7})
	require.NoError(err)
	assert.Equal([]int64{7}, ids)
	_, err = resolveChatwootSyncInboxes(st, src, []int64{8})
	require.ErrorContains(err, "excluded")
	_, err = resolveChatwootSyncInboxes(st, src, []int64{99})
	require.ErrorContains(err, "not registered")
}

func TestChatwootOptionsPreserveSourcePolicies(t *testing.T) {
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	no := false
	src := config.ChatwootSource{SelfAgentIDs: []int64{201}, IncludePrivate: &no, Media: &no, MaxMediaMB: 3, ReconcileIntervalHours: 2}
	opts := chatwootImportOptions(src, 7, cfg)
	assert.Equal(int64(7), opts.InboxID)
	assert.Equal([]int64{201}, opts.SelfAgentIDs)
	assert.False(opts.IncludePrivate)
	assert.False(opts.Media)
	assert.Equal(int64(3<<20), opts.MaxMediaBytes)
	assert.Equal(2*time.Hour, opts.ReconcileInterval)
	assert.Equal(cfg.AttachmentsDir(), opts.AttachmentsDir)
}

func TestChatwootRegistrationUsesDaemonEnvironmentAndInboxFilters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("synthetic-token", r.Header.Get("Api_access_token"))
		assert.Equal("/api/v1/accounts/9/inboxes", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":[{"id":7,"name":"Support","channel_type":"Channel::Api","auth_token":"do-not-store"},{"id":8,"name":"Excluded","channel_type":"Channel::Api"}]}`))
	}))
	defer srv.Close()
	registered, err := registerChatwootProfile(context.Background(), st, config.ChatwootSource{Identifier: "support", URL: srv.URL, AccountID: 9, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN", ExcludeInboxes: []int64{8}})
	require.NoError(err)
	require.Len(registered, 1)
	assert.Equal(srv.URL+"/accounts/9/inboxes/7", registered[0].Identifier)
	sources, err := st.ListSources(chatwoot.SourceType)
	require.NoError(err)
	assert.Len(sources, 1)
}

func TestChatwootScheduledJobUsesAccountIdentityAndDefersMediaPacking(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newAttachmentMaintenanceFixture(t)
	var hash string
	sched := scheduler.New(func(context.Context, string) error { return nil }).WithLogger(fixture.maintenance.logger)
	t.Cleanup(func() { <-sched.Stop().Done() })
	src := config.ChatwootSource{Identifier: "renamed-profile", URL: "https://chatwoot.example.com/support", AccountID: 9, Enabled: true, Schedule: "*/30 * * * *"}
	calls := 0
	require.NoError(registerScheduledChatwootJob(sched, src, fixture.maintenance, func(context.Context) error {
		calls++
		hash = fixture.ingestLoose([]byte("synthetic Chatwoot scheduled media"))
		return nil
	}))
	job, ok := api.SchedulerJobNameForSource(chatwoot.SourceType, "https://chatwoot.example.com/support/accounts/9/inboxes/7")
	require.True(ok)
	require.True(sched.IsJobScheduled(job))
	require.NoError(sched.TriggerJob(job))
	assert.Equal(1, calls)
	assert.Nil(fixture.packedEntry(hash), "scheduled sync defers packing")
	assert.True(fixture.maintenance.packPending.Load(), "new media requests a later pack pass")
	require.NoError(fixture.maintenance.runPendingPack(context.Background()))
	assert.NotNil(fixture.packedEntry(hash))
}

func TestChatwootProfileSyncContinuesAfterInboxFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	st := testutil.NewTestStore(t)
	t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
	var enumerations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/accounts/9/agents":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v1/accounts/9/conversations":
			if enumerations.Add(1) == 1 {
				http.Error(w, "synthetic access failure", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"payload":[]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	src := config.ChatwootSource{Identifier: "support", URL: server.URL, AccountID: 9, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN"}
	for _, id := range []int64{7, 8} {
		_, err := st.GetOrCreateSource(chatwoot.SourceType, chatwoot.SourceIdentifier(src.URL, 9, id))
		require.NoError(err)
	}
	sum, err := importChatwootProfile(context.Background(), st, src, chatwootRunOptions{}, cfg)
	require.ErrorContains(err, "inbox 7")
	assert.NotContains(err.Error(), "inbox 8")
	require.NotNil(sum)
	assert.Equal(2, sum.Sources)
	assert.Equal(int32(3), enumerations.Load(), "healthy inbox is listed by activity and enumerated after the failure")
}

func TestChatwootLimitedProfileSyncReportsResumableWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	st := testutil.NewTestStore(t)
	t.Setenv("EXAMPLE_CHATWOOT_TOKEN", "synthetic-token")
	messages := []chatwoot.Message{
		{ID: 101, InboxID: 7, ConversationID: 42, Content: "First synthetic message", ContentType: "text", CreatedAt: 1700000000},
		{ID: 102, InboxID: 7, ConversationID: 42, Content: "Second synthetic message", ContentType: "text", CreatedAt: 1700000001},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/accounts/9/agents":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v1/accounts/9/conversations":
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`{"data":{"payload":[{"id":42,"inbox_id":7,"status":"resolved","created_at":1700000000,"last_activity_at":1700000001,"messages":[{"id":102}]}]}}`))
			} else {
				_, _ = w.Write([]byte(`{"data":{"payload":[]}}`))
			}
		case "/api/v1/accounts/9/conversations/42/messages":
			after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			if !assert.NoError(err) {
				return
			}
			before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
			if !assert.NoError(err) {
				return
			}
			// Match the verified account API's inclusive after / exclusive before contract.
			page := []chatwoot.Message{}
			for _, message := range messages {
				if message.ID >= after && message.ID < before {
					page = append(page, message)
				}
			}
			body, err := json.Marshal(map[string]any{"payload": page})
			if !assert.NoError(err) {
				return
			}
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	src := config.ChatwootSource{Identifier: "support", URL: server.URL, AccountID: 9, APIKeyEnv: "EXAMPLE_CHATWOOT_TOKEN"}
	_, err := st.GetOrCreateSource(chatwoot.SourceType, chatwoot.SourceIdentifier(src.URL, 9, 7))
	require.NoError(err)
	sum, err := importChatwootProfile(context.Background(), st, src, chatwootRunOptions{Limit: 1}, cfg)
	require.NoError(err)
	require.NotNil(sum)
	assert.True(sum.Partial)
	assert.Equal(1, sum.MessagesProcessed)
	var output bytes.Buffer
	printChatwootSummary(&output, src.Identifier, sum)
	assert.Contains(output.String(), "1 messages processed (1 added)")
	assert.Contains(output.String(), "Unfinished history or artifact work will resume on the next sync.")
}
