package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type bindingSyncRemote struct {
	cards map[string]store.CardDAVRemoteResource
}

func (r *bindingSyncRemote) Discover(context.Context, string) (carddav.Discovery, error) {
	return carddav.Discovery{}, nil
}
func (r *bindingSyncRemote) Pull(_ context.Context, _ store.CardDAVAddressBook, _ string, _ *carddav.Budget) (store.CardDAVSyncPlan, error) {
	plan := store.CardDAVSyncPlan{ReplaceAll: true}
	for _, resource := range r.cards {
		plan.Upserts = append(plan.Upserts, resource)
	}
	return plan, nil
}
func (r *bindingSyncRemote) Get(context.Context, string) (store.CardDAVRemoteResource, bool, error) {
	return store.CardDAVRemoteResource{}, false, nil
}
func (r *bindingSyncRemote) Put(context.Context, string, []byte, string, bool) error { return nil }
func (r *bindingSyncRemote) Delete(context.Context, string, string) error            { return nil }
func (r *bindingSyncRemote) CreateHref(string, string) (string, error)               { return "", nil }
func (r *bindingSyncRemote) Limits() (time.Duration, int64)                          { return time.Minute, 10 << 20 }

func TestScheduledCardDAVSyncPersistsBindingConflictCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://memory.test", Username: "alice", PrincipalURL: "https://memory.test/principal/", HomeURL: "https://memory.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://memory.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}},
	})
	require.NoError(err)
	_, err = st.EnsureParticipant("shared@example.test", "Shared", "example.test")
	require.NoError(err)

	cards := make(map[string]store.CardDAVRemoteResource)
	for _, uid := range []string{"first", "second"} {
		body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:Person\r\nEMAIL:shared@example.test\r\nEND:VCARD\r\n")
		hash, hashErr := carddav.SemanticHash(body)
		require.NoError(hashErr)
		href := books[0].CanonicalURL + uid + ".vcf"
		cards[href] = store.CardDAVRemoteResource{
			Href: href, RemoteUID: uid, RemoteETag: `"one"`, SemanticHash: hash,
			RemoteBody: body, DisplayName: "Person", Emails: []string{"shared@example.test"},
		}
	}
	service := carddav.NewRemoteService(st, &bindingSyncRemote{cards: cards})
	result, err := service.Sync(t.Context(), carddav.SyncOptions{Trigger: store.CardDAVSyncTriggerScheduled})
	require.NoError(err)
	require.NotNil(result.BindingConflicts)
	assert.Equal(1, *result.BindingConflicts)

	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
	controller := &CardDAVController{cfg: cfg, store: st}
	response := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/runs")
	require.Equal(200, response.Code, response.Body.String())
	var body struct {
		Runs []map[string]any `json:"runs"`
	}
	require.NoError(json.NewDecoder(response.Body).Decode(&body))
	require.Len(body.Runs, 1)
	assert.Equal("scheduled", body.Runs[0]["trigger"])
	assert.InDelta(float64(1), body.Runs[0]["binding_conflicts"], 0)
}
