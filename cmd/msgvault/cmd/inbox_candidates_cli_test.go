package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Removing the command's source/cursor forwarding breaks real daemon pagination.
func TestInboxCLICandidatesRealDaemonPagination(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	source := inboxcontrol.SourceIdentity{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier}
	for _, id := range []string{"candidate-one", "candidate-two"} {
		mid := f.CreateMessage(id)
		_, err := f.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: inboxcontrol.Target{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: id}, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()})
		requirements.NoError(err)
	}
	server := httptest.NewServer(api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler)).Router())
	defer server.Close()
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true}})
	args := []string{"candidates", "--source-id", strconv.FormatInt(source.SourceID, 10), "--source-type", source.SourceType, "--source-identifier", source.SourceIdentifier, "--account-id", source.AccountID, "--scope", "message", "--limit", "1"}
	call := func(extra ...string) (inboxcontrol.CandidatePage, error) {
		command := newInboxCmd()
		command.SetContext(ctx)
		command.SetArgs(append(append([]string{}, args...), extra...))
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(io.Discard)
		err := command.Execute()
		if err != nil {
			return inboxcontrol.CandidatePage{}, fmt.Errorf("execute inbox candidates command: %w", err)
		}
		var page inboxcontrol.CandidatePage
		requirements.NoError(json.Unmarshal(output.Bytes(), &page))
		return page, nil
	}
	first, err := call()
	requirements.NoError(err)
	requirements.Len(first.Candidates, 1)
	requirements.NotEmpty(first.NextCursor)
	assertions.Equal(source, first.Source)
	second, err := call("--cursor", first.NextCursor)
	requirements.NoError(err)
	requirements.Len(second.Candidates, 1)
	assertions.Empty(second.NextCursor)
	assertions.NotEqual(first.Candidates[0].State.Target, second.Candidates[0].State.Target)
	f.CreateMessage("incoming-unobserved")
	_, err = call("--cursor", first.NextCursor)
	requirements.ErrorIs(err, inboxcontrol.ErrConflict)
	_, err = call("--limit", "101")
	requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	_, err = call("--scope", "chat")
	requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	_, err = call("--account-id", "foreign@example.test")
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	_, err = call("--apply")
	assertions.Error(err)
}
