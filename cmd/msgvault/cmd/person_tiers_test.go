package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

// tiersStore serves a canned preview and explanation and records the arguments
// it was called with.
type tiersStore struct {
	preview     store.PersonTierPreview
	explanation *store.PersonTierExplanation
	personID    int64
}

func (s *tiersStore) PreviewPersonTiersContext(context.Context, store.PersonTierOptions) (*store.PersonTierPreview, error) {
	return &s.preview, nil
}

func (s *tiersStore) ExplainPersonTierContext(_ context.Context, personID int64, _ store.PersonTierOptions) (*store.PersonTierExplanation, error) {
	s.personID = personID
	return s.explanation, nil
}

func tierRow(id int64, name string, tier int, tracked bool) store.PersonTierRow {
	rule := map[int]string{1: "tier1_intensity", 2: "tier2_two_way"}[tier]
	return store.PersonTierRow{PersonID: id, DisplayName: &name, Tier: tier, Rule: rule, Tracked: tracked}
}

// readyPreview lists rows in the order the store returns them: tier first,
// then newest counted day, so the IDs here are deliberately not ascending.
func readyPreview(rows ...store.PersonTierRow) store.PersonTierPreview {
	return store.PersonTierPreview{
		AsOf: "2026-10-17", Readiness: store.PersonTierReadiness{Ready: true, Reasons: []string{}}, People: rows,
		Projection: store.PersonTierProjection{Timezone: "UTC", MaxDirect: 25},
	}
}

var tiersTestOptions = store.PersonTierOptions{
	Config: persontiers.DefaultConfig(), ActivityTimezone: "UTC", ActivityMaxDirect: 25,
}

func daemonTiersDeps(st personTiersCommandStore) personTiersCommandDeps {
	return personTiersCommandDeps{
		options:            func() (store.PersonTierOptions, error) { return tiersTestOptions, nil },
		openStore:          func() (personTiersCommandStore, func(), error) { return st, func() {}, nil },
		isDaemonSubprocess: func() bool { return true },
	}
}

func executePersonTiersCommand(t *testing.T, deps personTiersCommandDeps, args ...string) (string, string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault", SilenceUsage: true, SilenceErrors: true}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonTiersCommand(deps))
	root.AddCommand(person)
	root.SetArgs(append([]string{"person", "tiers"}, args...))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(t.Context())
	return stdout.String(), stderr.String(), err
}

func TestPersonTiersPreviewPrintsJSON(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := &tiersStore{preview: readyPreview(tierRow(3, "Cleo", 1, false), tierRow(1, "Ada", 2, true))}
	stdout, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview", "--json")
	require.NoError(err)
	var out store.PersonTierPreview
	require.NoError(json.Unmarshal([]byte(stdout), &out))
	require.Len(out.People, 2)
	assert.Equal(int64(3), out.People[0].PersonID)
	assert.True(out.People[1].Tracked)
	assert.Equal("2026-10-17", out.AsOf)
}

func TestPersonTiersPreviewPrintsATable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	last := "2026-10-16"
	row := tierRow(3, "Cleo", 1, false)
	row.LastContactDate = &last
	st := &tiersStore{preview: readyPreview(row)}
	st.preview.Projection = store.PersonTierProjection{Timezone: "America/New_York", MaxDirect: 25}
	stdout, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview")
	require.NoError(err)
	assert.Contains(stdout, "counts built with timezone America/New_York, direct limit 25")
	assert.Regexp(`LAST COUNTED`, stdout)
	assert.Regexp(`1\s+3\s+Cleo\s+tier1_intensity\s+false\s+2026-10-16`, stdout)
}

func TestPersonTiersWarnOnceWhenReadinessIsNotReady(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &tiersStore{preview: readyPreview(tierRow(3, "Cleo", 1, false))}
	st.preview.Readiness = store.PersonTierReadiness{Reasons: []string{"watermark_behind", "settings_changed: stored timezone UTC and direct limit 25, configured UTC and 1"}}
	stdout, stderr, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview", "--json")
	require.NoError(err)
	assert.Equal(1, strings.Count(stderr, "Warning:"))
	assert.Contains(stderr, "watermark_behind")
	assert.Contains(stderr, "settings_changed")
	assert.True(json.Valid([]byte(stdout)), "stdout stays JSON")
}

func TestPersonTiersForwardTheCommandFromTheCLI(t *testing.T) {
	require := require.New(t)
	var forwarded [][]string
	deps := personTiersCommandDeps{
		isDaemonSubprocess: func() bool { return false },
		proxy: func(command *cobra.Command, runArgs []string) error {
			forwarded = append(forwarded, runArgs)
			_, _ = fmt.Fprintln(command.OutOrStdout(), "forwarded")
			return nil
		},
	}
	stdout, _, err := executePersonTiersCommand(t, deps, "preview", "--json")
	require.NoError(err)
	assert.Equal(t, "forwarded\n", stdout)
	_, _, err = executePersonTiersCommand(t, deps, "explain", "7")
	require.NoError(err)
	assert.Equal(t, [][]string{
		{"person", "tiers", "preview", "--json"},
		{"person", "tiers", "explain", "7"},
	}, forwarded)
}

func TestPersonTiersExplain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	name := "Seven"
	last := "2026-10-16"
	st := &tiersStore{explanation: &store.PersonTierExplanation{
		PersonID: 7, DisplayName: &name, Tier: 2, Rule: "tier2_two_way", Tracked: true, AsOf: "2026-10-17",
		LastContactDate: &last,
		Projection:      store.PersonTierProjection{Timezone: "UTC", MaxDirect: 25},
		Signals:         persontiers.Signals{ExcludedListEvents: 4, LargeAudienceInboundEvents: 9},
		Thresholds: []persontiers.ThresholdCheck{
			{Name: "tier2.min_active_days_long", Observed: 4, Required: 3, Enabled: true, Met: true},
			{Name: "tier2.min_meeting_days_long", Observed: 0, Required: 0},
		},
	}}
	stdout, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), "explain", "7")
	require.NoError(err)
	assert.Equal(int64(7), st.personID)
	assert.Contains(stdout, "Tier: 2")
	assert.Contains(stdout, "Last counted: 2026-10-16")
	assert.Contains(stdout, "Projection: timezone UTC, direct limit 25")
	assert.Contains(stdout, "Excluded list messages: 4")
	assert.Contains(stdout, "Large-audience inbound messages: 9")
	assert.Regexp(`tier2.min_active_days_long\s+4\s+3\s+true`, stdout)
	assert.Regexp(`tier2.min_meeting_days_long\s+0\s+off\s+false`, stdout)
	stdout, _, err = executePersonTiersCommand(t, daemonTiersDeps(st), "explain", "7", "--json")
	require.NoError(err)
	var out store.PersonTierExplanation
	require.NoError(json.Unmarshal([]byte(stdout), &out))
	assert.Equal(int64(7), out.PersonID)
}

func TestPersonTiersExplainRejectsBadIDBeforeTheDaemon(t *testing.T) {
	called := false
	deps := personTiersCommandDeps{
		isDaemonSubprocess: func() bool { return false },
		proxy:              func(*cobra.Command, []string) error { called = true; return nil },
	}
	_, _, err := executePersonTiersCommand(t, deps, "explain", "0")
	require.ErrorContains(t, err, "positive")
	assert.False(t, called)
}

// The tier thresholds and [activity] settings reach the store only through
// bind, so this runs the real command against a loaded config and a seeded
// archive. Mail on three days with both directions is tier 2 by default.
func TestPersonTiersUseTheLoadedConfig(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	home := t.TempDir()
	st, err := store.OpenForTest(filepath.Join(home, "msgvault.db"))
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	conversation, err := st.EnsureConversation(source.ID, "tier-thread", "Tier")
	require.NoError(t, err)
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(t, err)
	friend, err := st.EnsureParticipant("friend@example.com", "Friend", "example.com")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(friend)
	require.NoError(t, err)
	for day := 1; day <= 3; day++ {
		from, to := friend, owner
		if day == 1 {
			from, to = owner, friend
		}
		id, err := st.UpsertMessage(&store.Message{
			ConversationID: conversation, SourceID: source.ID, MessageType: "email",
			SourceMessageID: fmt.Sprintf("tier-%d", day),
			SentAt:          sql.NullTime{Time: time.Now().UTC().AddDate(0, 0, -day), Valid: true},
			SenderID:        sql.NullInt64{Int64: from, Valid: true},
		})
		require.NoError(t, err)
		require.NoError(t, st.ReplaceMessageRecipients(id, "from", []int64{from}, []string{""}))
		require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{to}, []string{""}))
	}
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: 25})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
	require.NoError(t, st.Close())

	run := func(t *testing.T, toml string) (store.PersonTierPreview, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(toml), 0o600))
		cfg, err := config.Load(path, home)
		require.NoError(t, err)
		command := &cobra.Command{Use: "msgvault", SilenceUsage: true, SilenceErrors: true}
		person := &cobra.Command{Use: "person"}
		person.AddCommand(newPersonTiersCommand(defaultPersonTiersCommandDeps()))
		command.AddCommand(person)
		command.SetArgs([]string{"person", "tiers", "preview", "--json"})
		var stdout, stderr bytes.Buffer
		command.SetOut(&stdout)
		command.SetErr(&stderr)
		require.NoError(t, command.ExecuteContext(testInvocationContext(t.Context(), cfg, invocationOptions{})))
		var preview store.PersonTierPreview
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &preview))
		return preview, stderr.String()
	}

	preview, _ := run(t, "")
	require.Len(t, preview.People, 1)
	assert.Equal(t, person.ID, preview.People[0].PersonID)
	assert.Equal(t, 2, preview.People[0].Tier)

	preview, _ = run(t, "[people.tiers.tier2]\nmin_active_days_long = 4\n")
	assert.Empty(t, preview.People, "a raised threshold from [people.tiers] drops the person")

	_, warning := run(t, "[activity]\ntimezone = \"America/New_York\"\n")
	assert.Contains(t, warning, "settings_changed")
}
