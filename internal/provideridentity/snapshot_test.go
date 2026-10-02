package provideridentity_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fastmail"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type jmapTransport struct{ target *url.URL }

func (t jmapTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// Synthetic snapshots exercise the shared store contract; this does not imply
// an API discovery adapter is available for the second provider.
func TestMaskedEmailCategoryKeepsProviderProvenanceAndMatchesLocally(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant("mask@example.test", "Synthetic", "example.test")
	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message:    &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "masked-from", MessageType: "email"},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, DisplayNames: []string{"Synthetic"}, EmailAddresses: []string{"mask@example.test"}}},
	})
	require.NoError(err)
	otherSource, err := f.Store.GetOrCreateSource("imap", "other@example.test")
	require.NoError(err)
	for _, provider := range []string{"fastmail", "simplelogin"} {
		record := store.ProviderIdentityRecord{ID: "same-provider-object-id", Identifier: "mask@example.test", Kind: "masked-email", State: "enabled", Description: provider}
		snapshot := provideridentity.Snapshot{
			Provider: provider, State: "first", Records: []store.ProviderIdentityRecord{record},
			Evidence: []identityops.ExternalEvidence{
				{Identifier: record.Identifier, Signal: identityops.SignalProviderAlias, Strong: true},
				{Identifier: record.Identifier, Signal: identityops.SignalMaskedEmail, Strong: true},
			},
		}
		_, changed, err := provideridentity.ApplySnapshot(t.Context(), f.Store, f.Source.ID, snapshot)
		require.NoError(err)
		assert.True(changed)
		outcomes, changed, err := provideridentity.ApplySnapshot(t.Context(), f.Store, f.Source.ID, snapshot)
		require.NoError(err)
		assert.False(changed)
		assert.Empty(outcomes)
	}
	for _, provider := range []string{"fastmail", "simplelogin"} {
		records, err := f.Store.ListProviderIdentityRecordsContext(t.Context(), f.Source.ID, provider)
		require.NoError(err)
		require.Len(records, 1)
		assert.Equal("masked-email", records[0].Kind)
		assert.Equal(provider, records[0].Description, "identical object IDs remain provider-scoped")
		records, err = f.Store.ListProviderIdentityRecordsContext(t.Context(), otherSource.ID, provider)
		require.NoError(err)
		assert.Empty(records)
	}
	identities, err := f.Store.ListAccountIdentities(f.Source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("masked-email,provider-alias", identities[0].SourceSignal)
	identities, err = f.Store.ListAccountIdentities(otherSource.ID)
	require.NoError(err)
	assert.Empty(identities)
	fromMe, err := f.Store.GetMessageIsFromMe(messageID)
	require.NoError(err)
	assert.True(fromMe, "the stored confirmed identity repairs local ownership without a provider read")
}

func TestFastmailSnapshotPipelineAtScale(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "inbox@example.test")
	require.NoError(err)
	var calls atomic.Int64
	var revision atomic.Int64
	revision.Store(1)
	var count atomic.Int64
	count.Store(3000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"apiUrl": "/jmap", "capabilities": map[string]any{fastmail.CoreCapability: map[string]any{"maxObjectsInGet": 4096}, fastmail.MaskedEmailCapability: map[string]any{}}, "accounts": map[string]any{"account": map[string]any{"accountCapabilities": map[string]any{fastmail.MaskedEmailCapability: map[string]any{}}}}, "primaryAccounts": map[string]string{fastmail.MaskedEmailCapability: "account"}}))
			return
		}
		if count.Load() > 4096 {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{"error", map[string]any{"type": "requestTooLarge"}, "masked"}}}))
			return
		}
		calls.Add(1)
		states := []string{"enabled", "disabled", "deleted", "pending"}
		list := make([]any, count.Load())
		for i := range list {
			state := states[i%4]
			if revision.Load() > 1 && i == 0 {
				state = "deleted"
			}
			list[i] = map[string]any{"id": strconv.Itoa(i), "email": fmt.Sprintf("mask-%04d@example.test", i), "state": state, "forDomain": "https://example.com", "description": "Synthetic", "createdAt": "2026-01-01T00:00:00Z"}
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{"MaskedEmail/get", map[string]any{"accountId": "account", "state": strconv.FormatInt(revision.Load(), 10), "list": list}, "masked"}}}))
	}))
	assert := assert.New(t)
	defer srv.Close()
	target, err := url.Parse(srv.URL)
	require.NoError(err)
	factory := func(token string) provideridentity.Inventory {
		return fastmail.NewClient(token, &http.Client{Transport: jmapTransport{target}})
	}
	cfg := &config.Config{Fastmail: []config.FastmailSource{{SourceID: source.ID, APIToken: "synthetic-token", AutoConfirmIdentities: true}}}
	outcomes, enabled, err := provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	require.NoError(err)
	assert.True(enabled)
	assert.Len(outcomes, 2250)
	for _, outcome := range outcomes {
		assert.Equal([]string{"masked-email", "provider-alias"}, outcome.Signals)
	}
	records, err := st.ListProviderIdentityRecordsContext(t.Context(), source.ID, "fastmail")
	require.NoError(err)
	require.Len(records, 3000)
	for _, record := range records {
		assert.Equal("masked-email", record.Kind)
	}
	otherProviderRecords, err := st.ListProviderIdentityRecordsContext(t.Context(), source.ID, "simplelogin")
	require.NoError(err)
	assert.Empty(otherProviderRecords, "provider provenance remains separate from the category")
	before, found, err := st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	require.True(found)
	outcomes, _, err = provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	require.NoError(err)
	assert.Empty(outcomes)
	after, _, err := st.ProviderIdentityRefreshStateContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Equal(before, after, "unchanged state must not write refresh metadata")
	// A successful unchanged poll also suppresses further idle polls even if
	// the durable marker says retry: the no-op itself still writes nothing.
	require.NoError(st.RecordProviderIdentityRefreshOutcomeContext(t.Context(), source.ID, errors.New("synthetic failure")))
	st.NoteProviderIdentityCheck(source.ID, false)
	_, _, err = provideridentity.AutoRefreshIfDue(t.Context(), cfg, st, source.ID, factory)
	require.NoError(err)
	polled := calls.Load()
	_, _, err = provideridentity.AutoRefreshIfDue(t.Context(), cfg, st, source.ID, factory)
	require.NoError(err)
	assert.Equal(polled, calls.Load(), "successful no-op keeps idle polling fresh without a database write")
	revision.Store(2)
	count.Store(2998)
	_, _, err = provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	require.NoError(err)
	records, err = st.ListProviderIdentityRecordsContext(t.Context(), source.ID, "fastmail")
	require.NoError(err)
	require.Len(records, 3000)
	removed := 0
	for _, r := range records {
		if r.Removed {
			removed++
		}
		if r.Identifier == "mask-0000@example.test" {
			assert.Equal("deleted", r.State)
		}
	}
	assert.Equal(2, removed)
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Len(identities, 2250)
	count.Store(4097)
	_, _, err = provideridentity.AutoRefresh(t.Context(), cfg, st, source.ID, factory)
	require.ErrorContains(err, "4096")
	current, readErr := st.ListProviderIdentityRecordsContext(t.Context(), source.ID, "fastmail")
	require.NoError(readErr)
	assert.Equal(records, current)
	currentIdentities, readErr := st.ListAccountIdentities(source.ID)
	require.NoError(readErr)
	assert.Equal(identities, currentIdentities)
}
