package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func runLoops(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := newLoopsCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	command.SetContext(ctx)
	err := command.Execute()
	return output.String(), err
}

func loopsDaemon(t *testing.T) (context.Context, int64, int64) {
	t.Helper()
	f := storetest.New(t)
	now := time.Now().UTC()
	f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: "Hi", SentAt: now.Add(-10 * 24 * time.Hour)})
	f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Subject: "Contract draft", Body: "Can you send the contract?", SentAt: now.Add(-3 * 24 * time.Hour)})
	anchor := f.PersistEmail(storetest.EmailSpec{Thread: "waiting", From: storetest.OwnerAddress, To: []string{"sam@example.test"}, Subject: "Lunch Friday?", Body: "Are you free?", SentAt: now.Add(-5 * 24 * time.Hour)})
	participant := f.EnsureParticipant("alex@example.test", "", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	return withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}}), anchor, person.ID
}

func TestLoopsListReadsTheDaemonAndPassesLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx, _, personID := loopsDaemon(t)
	output, err := runLoops(ctx, t, "list")
	require.NoError(err)
	assert.Contains(output, "alex@example.test")
	assert.Contains(output, "Contract draft")

	output, err = runLoops(ctx, t, "list", "--limit", "1", "--json")
	require.NoError(err)
	var decoded generated.LoopListResponse
	require.NoError(json.Unmarshal([]byte(output), &decoded))
	require.Len(decoded.Items, 1)
	assert.True(decoded.Truncated, "--limit reached the daemon")
	output, err = runLoops(ctx, t, "list", "--person", strconv.FormatInt(personID, 10), "--json")
	require.NoError(err)
	var filtered generated.LoopListResponse
	require.NoError(json.Unmarshal([]byte(output), &filtered))
	require.Len(filtered.Items, 1)
	require.NotNil(filtered.Items[0].Counterparty.PersonID)
	assert.Equal(personID, *filtered.Items[0].Counterparty.PersonID)
}

func TestLoopsListRejectsBadFlags(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	for _, args := range [][]string{
		{"list", "--limit", "201"}, {"list", "--limit", "0"},
		{"list", "--person", "0"}, {"list", "--person", "-1"},
		{"list", "--person", "abc"}, {"list", "--person", "9223372036854775808"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require := require.New(t)
			_, err := runLoops(ctx, t, args...)
			require.Error(err)
			assert.Zero(t, requests, "invalid flags are rejected before contacting the daemon")
		})
	}
}

func TestLoopsDoneAndSnooze(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx, anchor, _ := loopsDaemon(t)

	output, err := runLoops(ctx, t, "snooze", strconv.FormatInt(anchor, 10), "--until", "48h")
	require.NoError(err)
	assert.Regexp(fmt.Sprintf(`^Snoozed %d until \w{3} \w{3} \d{1,2} \d{1,2}:\d{2} [AP]M \S+\.\n$`, anchor), output)

	missing := anchor + 10000
	_, err = runLoops(ctx, t, "done", strconv.FormatInt(missing, 10))
	require.EqualError(err, fmt.Sprintf("%d isn't an open loop.", missing))

	ctx, anchor, _ = loopsDaemon(t)
	output, err = runLoops(ctx, t, "done", strconv.FormatInt(anchor, 10))
	require.NoError(err)
	assert.Equal(fmt.Sprintf("Marked %d done.\n", anchor), output)

	_, err = runLoops(ctx, t, "snooze", strconv.FormatInt(anchor, 10), "--until", "yesterday")
	require.Error(err)
	_, err = runLoops(ctx, t, "done", "abc")
	require.Error(err)
}

func TestLoopsRequireDaemonSchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var loopRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.23.0"}`))
			return
		}
		loopRequests++
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	_, err := runLoops(ctx, t, "list")
	require.ErrorContains(err, "require daemon API schema 3.24.0")
	require.ErrorContains(err, `daemon reports "3.23.0"`)
	assert.Zero(loopRequests)
}

func TestWriteLoopsLayout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	item := func(kind, channel, summary string, ago time.Duration, name, address string, id int64) generated.LoopItem {
		return generated.LoopItem{
			Kind: generated.LoopItemKind(kind), Channel: generated.LoopItemChannel(channel), Summary: summary,
			OwedSinceAt: now.Add(-ago), AnchorMessageID: id,
			Counterparty: generated.LoopCounterparty{DisplayName: name, Address: address},
		}
	}
	var out strings.Builder
	require.NoError(writeLoops(&out, generated.LoopListResponse{Truncated: true, Items: []generated.LoopItem{
		item("awaiting_reply", "email", "Lunch Friday?", 120*time.Hour, "Sam Rivera", "", 4380),
		item("reply_owed", "email", "Contract draft", 72*time.Hour, "Alex Example", "alex@example.test", 4521),
	}}, 50, now))
	assert.Equal(`Waiting on you
  3d   Alex Example <alex@example.test>   Contract draft   4521
Waiting on them
  5d   Sam Rivera                         Lunch Friday?    4380

Showing the oldest 50. Use --limit up to 200 to see more.
`, out.String(), "a truncated page does not claim totals")

	out.Reset()
	require.NoError(writeLoops(&out, generated.LoopListResponse{Truncated: true, Items: []generated.LoopItem{
		item("awaiting_reply", "email", "Lunch Friday?", 120*time.Hour, "Sam Rivera", "", 4380),
	}}, loops.MaxLimit, now))
	assert.Contains(out.String(), "Showing the oldest 200. Results are capped at 200.\n")

	out.Reset()
	require.NoError(writeLoops(&out, generated.LoopListResponse{SkippedUnparseable: 2, Items: []generated.LoopItem{
		item("reply_owed", "email", "Contract draft", 72*time.Hour, "Alex Example", "alex@example.test", 4521),
	}}, 50, now))
	assert.Equal(`Waiting on you
  3d   Alex Example <alex@example.test>   Contract draft   4521

1 waiting on you, 0 waiting on them.
2 stored messages could not be read, so a reply in one may have settled a loop.
`, out.String())

	out.Reset()
	require.NoError(writeLoops(&out, generated.LoopListResponse{Items: []generated.LoopItem{
		item("reply_owed", "chat", strings.Repeat("x", 80), 5*time.Hour, "", "+15550100", 4600),
	}}, 50, now))
	assert.Contains(out.String(), "  5h   +15550100   "+strings.Repeat("x", 57)+"...   4600\n", "a chat snippet is cut to 60 runes")

	out.Reset()
	require.NoError(writeLoops(&out, generated.LoopListResponse{}, 50, now))
	assert.Equal("No open loops.\n", out.String(), "no line about unreadable messages when there are none")

	out.Reset()
	require.NoError(writeLoops(&out, generated.LoopListResponse{SkippedUnparseable: 1}, 50, now))
	assert.Equal("No open loops.\n1 stored message could not be read, so a reply in it may have settled a loop.\n", out.String())

	out.Reset()
	require.NoError(writeLoops(&out, generated.LoopListResponse{CoverageIncomplete: true}, 50, now))
	assert.Equal("No open loops.\nMore history remains. Run the list again to continue checking; these results cover checked conversations.\n", out.String())

	out.Reset()
	require.NoError(writeLoops(&out, generated.LoopListResponse{Items: []generated.LoopItem{
		item("awaiting_reply", "email", "", 80*time.Hour, "", "", 1),
	}}, 50, now))
	assert.Contains(out.String(), "(unknown)   (no subject)")
	assert.NotContains(out.String(), "Waiting on you")
}
