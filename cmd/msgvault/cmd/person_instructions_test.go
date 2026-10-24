package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func runInstructionCommand(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	cmd := newPersonInstructionsCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	err := cmd.Execute()
	return output.String(), err
}
func TestPersonInstructionsAddListRevokeThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipantByIdentifier("email", "alice@example.com", "alice")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	var requests atomic.Int64
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); srv.Router().ServeHTTP(w, r) }))
	t.Cleanup(httpSrv.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: httpSrv.URL, AllowInsecure: true}})
	personID := strconv.FormatInt(person.ID, 10)
	for _, args := range [][]string{
		{"add", personID, "--kind", "fax", "--text", "avoid calls"},
		{"add", personID, "--kind", "timing", "--text", "avoid calls", "--until", time.Now().UTC().AddDate(0, 0, 185).Format(time.DateOnly)},
	} {
		_, err := runInstructionCommand(t, ctx, args...)
		require.Error(err)
	}
	assert.Zero(requests.Load())
	output, err := runInstructionCommand(t, ctx, "add", personID, "--kind", "address_as", "--text", "call me Vee", "--value", "Vee")
	require.NoError(err)
	assert.Contains(output, "address_as")
	assert.Contains(output, "active")
	rows, err := st.ListPersonInstructionsContext(t.Context(), person.ID, store.PersonInstructionFilter{})
	require.NoError(err)
	require.Len(rows, 1)
	output, err = runInstructionCommand(t, ctx, "list", personID)
	require.NoError(err)
	assert.Contains(output, "ID")
	assert.Contains(output, "call me vee")
	output, err = runInstructionCommand(t, ctx, "revoke", strconv.FormatInt(rows[0].ID, 10), "--reason", "changed")
	require.NoError(err)
	assert.Contains(output, "revoked")
	row, err := st.GetPersonInstructionContext(t.Context(), rows[0].ID)
	require.NoError(err)
	assert.Equal(personfacts.InstructionStatusRevoked, row.Status)
	output, err = runInstructionCommand(t, ctx, "list", personID, "--all", "--json")
	require.NoError(err)
	assert.Contains(output, `"revoked"`)
	// Proposals are written by a later extraction program; seed one to exercise
	// the real confirmation HTTP path and its advisory-only follow-on output.
	var proposal int64
	now := time.Now().UTC()
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO person_instructions(person_id,instruction_key,kind,normalized_text,value_text,origin,status,stated_at,valid_from) VALUES (?,?,'address_as','call me Val','Val','extraction','proposed',?,?) RETURNING id`), person.ID, personfacts.InstructionKey(personfacts.InstructionKindAddressAs, "", "Val", "call me Val"), now, now).Scan(&proposal))
	output, err = runInstructionCommand(t, ctx, "confirm", fmt.Sprint(proposal))
	require.NoError(err)
	assert.Contains(output, "Suggested: save 'Val' as a nickname")
	profile, err := st.GetPersonProfileContext(t.Context(), person.ID)
	require.NoError(err)
	for _, name := range profile.Names {
		assert.NotEqual(store.PersonNameNickname, name.NameKind)
	}
}

func TestPersonInstructionsReviewIdentifiesPersonAndScopeThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var instructions []*store.PersonInstruction
	for _, name := range []string{"alice", "bob"} {
		participant, err := st.EnsureParticipantByIdentifier("email", name+"@example.com", name)
		require.NoError(err)
		person, _, err := st.CreatePersonFromParticipant(participant)
		require.NoError(err)
		row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: person.ID, Kind: personfacts.InstructionKindTiming, Text: "no calls before 9am", Actor: "owner"})
		require.NoError(err)
		instructions = append(instructions, row)
		if name == "bob" {
			for _, address := range []string{"bob.work@example.com", "bob.home@example.com"} {
				point, err := st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: address, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
				require.NoError(err)
				row, err := st.AddPersonInstructionContext(t.Context(), store.PersonInstructionInput{PersonID: person.ID, Kind: personfacts.InstructionKindAvoidAddress, Text: "do not use this address", ContactPointID: &point.Envelope.ID, Actor: "owner"})
				require.NoError(err)
				instructions = append(instructions, row)
			}
		}
	}
	// Merge reconciliation flags existing instructions for owner review.
	_, err := st.DB().ExecContext(t.Context(), `UPDATE person_instructions SET review_state='merged'`)
	require.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)})
	httpSrv := httptest.NewServer(srv.Router())
	t.Cleanup(httpSrv.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: httpSrv.URL, AllowInsecure: true}})
	output, err := runInstructionCommand(t, ctx, "review")
	require.NoError(err)
	lines := strings.Split(strings.TrimSpace(output), "\n")
	require.Len(lines, len(instructions)+1)
	assert.Equal([]string{"ID", "PERSON", "SCOPE", "KIND", "STATUS", "REVIEW", "VALUE", "TEXT"}, strings.Fields(lines[0]))
	for _, row := range instructions {
		var fields []string
		for _, line := range lines[1:] {
			candidate := strings.Fields(line)
			if candidate[0] == fmt.Sprint(row.ID) {
				fields = candidate
				break
			}
		}
		require.GreaterOrEqual(len(fields), 3)
		assert.Equal(fmt.Sprint(row.PersonID), fields[1])
		scope := row.ScopeKey
		if scope == "" {
			scope = "person"
		}
		assert.Equal(scope, fields[2])
	}
}
