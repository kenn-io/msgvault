package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func runPersonObservationsCommand(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := newPersonObservationsCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetContext(ctx)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}
func TestPersonObservationsAddListDecide(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("alice@example.com", "alice", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output, err := runPersonObservationsCommand(ctx, t, "add", strconv.FormatInt(person.ID, 10), "[person] swims", "--json")
	require.NoError(err, output)
	var row struct {
		ID         int64  `json:"id"`
		ContentKey string `json:"content_key"`
	}
	require.NoError(json.Unmarshal([]byte(output), &row))
	require.Positive(row.ID)
	output, err = runPersonObservationsCommand(ctx, t, "list", strconv.FormatInt(person.ID, 10))
	require.NoError(err, output)
	assert.Contains(output, "[PERSON] swims")
	ref := fmt.Sprintf("observation:%d", row.ID)
	assert.Contains(output, ref)
	output, err = runPersonObservationsCommand(ctx, t, "show", ref, "--json")
	require.NoError(err, output)
	assert.Contains(output, `"events"`)
	output, err = runPersonObservationsCommand(ctx, t, "decide", ref, "reject", "--reason", "wrong", "--json")
	require.NoError(err, output)
	assert.Contains(output, `"rejected"`)
	output, err = runPersonObservationsCommand(ctx, t, "suppressions", strconv.FormatInt(person.ID, 10), "--json")
	require.NoError(err, output)
	assert.Contains(output, row.ContentKey)
	output, err = runPersonObservationsCommand(ctx, t, "suppressions", "clear", strconv.FormatInt(person.ID, 10), row.ContentKey)
	require.NoError(err, output)
	output, err = runPersonObservationsCommand(ctx, t, "suppressions", strconv.FormatInt(person.ID, 10), "--json")
	require.NoError(err, output)
	assert.NotContains(output, row.ContentKey)
}
func TestPersonObservationsCommandRejectsInvalidReferences(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantError string
	}{
		{name: "zero observation ID", args: []string{"show", "observation:0"}, wantError: "observation must be a positive ID or observation:<id>"},
		{name: "negative observation ID", args: []string{"show", "observation:-1"}, wantError: "observation must be a positive ID or observation:<id>"},
		{name: "malformed observation reference", args: []string{"show", "nope"}, wantError: "observation must be a positive ID or observation:<id>"},
		{name: "non-positive person ID", args: []string{"list", "0"}, wantError: "person ID must be a positive integer"},
		{name: "unknown decision", args: []string{"decide", "observation:1", "unknown"}, wantError: "must be a valid DecidePersonObservationRequestAction value, got: unknown"},
		{name: "unknown category", args: []string{"add", "1", "[PERSON] runs", "--category", "unknown"}, wantError: "invalid category: unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			_, err := runPersonObservationsCommand(t.Context(), t, tc.args...)
			require.ErrorContains(err, tc.wantError)
		})
	}
}
