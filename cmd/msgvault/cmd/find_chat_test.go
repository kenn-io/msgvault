package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFindChatCommandReadsNamesAcrossNetworksThroughProductionAdapter(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	wantIDs := []int64{}
	var nativeSource int64
	for _, fixture := range []struct{ account, network, name string }{
		{"social-account", "Instagram", "Jordan Lee Chen"},
		{"native-account", "iMessage", "Lee Chen"},
	} {
		source, err := st.GetOrCreateSource("beeper", fixture.account)
		requirements.NoError(err)
		requirements.NoError(st.UpdateSourceDisplayName(source.ID, "Beeper "+fixture.network))
		conversation, err := st.EnsureConversationWithType(source.ID, fixture.name, "direct_chat", fixture.name)
		requirements.NoError(err)
		_, err = st.UpsertMessage(&store.Message{
			SourceID: source.ID, SourceMessageID: "fixture-message", ConversationID: conversation, MessageType: "beeper",
		})
		requirements.NoError(err)
		wantIDs = append(wantIDs, conversation)
		nativeSource = source.ID
	}
	server := api.NewServer(&config.Config{Server: config.ServerConfig{APIKey: "fixture-key"}},
		&storeAPIAdapter{store: st}, nil, slog.New(slog.DiscardHandler))
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{
		URL: httpServer.URL, APIKey: "fixture-key", AllowInsecure: true,
	}})
	command := newFindChatCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(io.Discard)
	command.SetContext(ctx)
	command.SetArgs([]string{"--json", "Jordan", "Lee", "Chen"})
	requirements.NoError(command.Execute())
	var page store.ChatDiscoveryPage
	requirements.NoError(json.Unmarshal(stdout.Bytes(), &page))
	requirements.Len(page.Results, 2)
	command = newFindChatCommand()
	stdout.Reset()
	command.SetOut(&stdout)
	command.SetErr(io.Discard)
	command.SetContext(ctx)
	command.SetArgs([]string{"--json", "--source-id", strconv.FormatInt(nativeSource, 10), "Jordan Lee Chen"})
	requirements.NoError(command.Execute())
	requirements.NoError(json.Unmarshal(stdout.Bytes(), &page))
	requirements.Len(page.Results, 1)
	assertions.Equal(wantIDs[1], page.Results[0].ConversationID)
	command = newFindChatCommand()
	stdout.Reset()
	command.SetOut(&stdout)
	command.SetErr(io.Discard)
	command.SetContext(ctx)
	command.SetArgs([]string{"Jordan Lee Chen"})
	requirements.NoError(command.Execute())
	assertions.Contains(stdout.String(), "Instagram")
	assertions.Contains(stdout.String(), "iMessage")
	assertions.Contains(stdout.String(), "MESSAGE")
	untitled, err := st.EnsureConversationWithType(nativeSource, "opaque-provider-42", "direct_chat", "")
	requirements.NoError(err)
	for i := range 10 {
		name := "Casey Member" + strconv.Itoa(i)
		if i == 0 {
			name = "Casey \x1b[31mMatch\x1b[0m"
		}
		peer, err := st.EnsureParticipant("peer"+strconv.Itoa(i)+"@example.test", name, "example.test")
		requirements.NoError(err)
		requirements.NoError(st.EnsureConversationParticipant(untitled, peer, ""))
	}
	_, err = st.UpsertMessage(&store.Message{SourceID: nativeSource, SourceMessageID: "untitled-message", ConversationID: untitled, MessageType: "beeper"})
	requirements.NoError(err)
	command = newFindChatCommand()
	stdout.Reset()
	command.SetOut(&stdout)
	command.SetErr(io.Discard)
	command.SetContext(ctx)
	command.SetArgs([]string{"Casey"})
	requirements.NoError(command.Execute())
	assertions.Contains(stdout.String(), "MATCHED")
	assertions.Contains(stdout.String(), "opaque-provider-42")
	assertions.Contains(stdout.String(), "Casey Match")
	assertions.Contains(stdout.String(), "…")
	assertions.NotContains(stdout.String(), "\x1b")

	// Provider-controlled metadata must not execute terminal escape sequences.
	_, err = st.DB().Exec(st.Rebind("UPDATE conversations SET title = ? WHERE id = ?"), "Lee \x1b[31mChen\x1b[0m", wantIDs[1])
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(nativeSource, "Beeper iMessage\x1b[31m"))
	command = newFindChatCommand()
	stdout.Reset()
	command.SetOut(&stdout)
	command.SetErr(io.Discard)
	command.SetContext(ctx)
	command.SetArgs([]string{"Lee"})
	requirements.NoError(command.Execute())
	assertions.NotContains(stdout.String(), "\x1b")
}

func TestFindChatCommandIsRegistered(t *testing.T) {
	command, remaining, err := rootCmd.Find([]string{"find-chat"})
	require.NoError(t, err)
	assert.Equal(t, "find-chat", command.Name())
	assert.Empty(t, remaining)
}

func TestFindChatCommandRejectsBadInputBeforeOpeningDaemon(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{
		URL: server.URL, AllowInsecure: true,
	}})
	for _, args := range [][]string{
		{}, {" "}, {"--limit", "0", "name"}, {"--source-id", "0", "name"},
	} {
		command := newFindChatCommand()
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		command.SetContext(ctx)
		command.SetArgs(args)
		require.Error(t, command.Execute())
	}
	assert.Zero(t, requests.Load(), "invalid input must fail before any daemon request")
}
