package cmd

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// Use the public generated SDK against the production daemon router and Store.
// Empty text must survive its response validator and JSON transport unchanged.
func TestInboxArchivedReadsGeneratedSDKNative(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	var targets []inboxcontrol.Target
	for _, providerID := range []string{"sdk-first", "sdk-second"} {
		mid := f.CreateMessage(providerID)
		target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: providerID}
		_, err := f.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()})
		requirements.NoError(err)
		_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET is_read=FALSE WHERE id=?`), mid)
		requirements.NoError(err)
		targets = append(targets, target)
	}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), targets[0].ItemID, "")
	requirements.NoError(err)
	server := httptest.NewServer(api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler)).Router())
	defer server.Close()
	sdk, err := client.New(server.URL)
	requirements.NoError(err)
	authorize := func(_ context.Context, request *http.Request) error {
		request.Header.Set("Authorization", "Bearer synthetic-owner-key")
		return nil
	}
	query := &generated.ListInboxCandidatesQuery{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: "message", Limit: new(int64(1))}
	first, err := sdk.ListInboxCandidates(t.Context(), &generated.ListInboxCandidatesRequestOptions{Query: query}, authorize)
	requirements.NoError(err)
	requirements.Len(first.Candidates, 1)
	requirements.NotNil(first.NextCursor)
	query.Cursor = first.NextCursor
	second, err := sdk.ListInboxCandidates(t.Context(), &generated.ListInboxCandidatesRequestOptions{Query: query}, authorize)
	requirements.NoError(err)
	requirements.Len(second.Candidates, 1)
	assertions.NotEqual(first.Candidates[0].State.Target.ItemID, second.Candidates[0].State.Target.ItemID)
	for i, target := range targets {
		result, err := sdk.GetInboxContext(t.Context(), &generated.GetInboxContextRequestOptions{Body: &generated.GetInboxContextBody{Target: generated.InboxTarget{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID, Scope: string(target.Scope), ItemID: target.ItemID, ProviderID: target.ProviderID}}}, authorize)
		requirements.NoError(err)
		requirements.NotNil(result.Text)
		assertions.Empty(*result.Text)
		assertions.Equal(i == 1, result.Unavailable)
		assertions.False(result.Truncated)
		var read bool
		requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT is_read FROM messages WHERE id=?`), target.ItemID).Scan(&read))
		assertions.False(read)
	}
	var receipts int
	requirements.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM inbox_operation_receipts`).Scan(&receipts))
	assertions.Zero(receipts)
}
