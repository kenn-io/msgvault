package api

import (
	"bytes"
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

type bindingSyncFixture struct {
	cardDAVListFixture
	result carddav.SyncResult
}

func (f bindingSyncFixture) Sync(context.Context, carddav.SyncOptions) (carddav.SyncResult, error) {
	return f.result, nil
}

func TestCardDAVSyncReportsBindingConflict(t *testing.T) {
	conflicts := 1
	controller := &CardDAVController{
		cfg:     &config.Config{CardDAV: config.CardDAVConfig{Enabled: true}},
		service: bindingSyncFixture{result: carddav.SyncResult{BindingConflicts: &conflicts}},
	}
	result, err := controller.Sync(t.Context(), CardDAVSyncRequest{Connection: "default"})
	require.NoError(t, err)
	require.NotNil(t, result.BindingConflicts)
	assert.Equal(t, 1, *result.BindingConflicts)
}

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
func (r *bindingSyncRemote) Get(_ context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	resource, ok := r.cards[href]
	return resource, !ok, nil
}
func (r *bindingSyncRemote) Put(_ context.Context, href string, body []byte, _ string, _ bool) error {
	hash, err := carddav.SemanticHash(body)
	if err != nil {
		return err
	}
	resource := store.CardDAVRemoteResource{Href: href, RemoteBody: body, RemoteETag: `"published"`, SemanticHash: hash, RemoteUID: "second", DisplayName: "Second"}
	if bytes.Contains(body, []byte("shared@example.test")) {
		resource.Emails = []string{"shared@example.test"}
	}
	r.cards[href] = resource
	return nil
}
func (r *bindingSyncRemote) Delete(_ context.Context, href, _ string) error {
	delete(r.cards, href)
	return nil
}
func (r *bindingSyncRemote) CreateHref(collectionURL, uid string) (string, error) {
	return collectionURL + uid, nil
}
func (r *bindingSyncRemote) Limits() (time.Duration, int64) { return time.Minute, 10 << 20 }

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
	assert.NotNil(result.BindingConflicts)
	if result.BindingConflicts != nil {
		assert.Equal(1, *result.BindingConflicts)
	}
	preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
	require.NoError(err)
	assert.Equal(1, preview.Conflicts)

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
	assert.Equal(float64(preview.Conflicts), body.Runs[0]["binding_conflicts"])
}
