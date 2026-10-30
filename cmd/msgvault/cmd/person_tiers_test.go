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
	return executePersonTiersCommandContext(t.Context(), deps, args...)
}

func executePersonTiersCommandContext(ctx context.Context, deps personTiersCommandDeps, args ...string) (string, string, error) {
	root := &cobra.Command{Use: "msgvault", SilenceUsage: true, SilenceErrors: true}
	person := &cobra.Command{Use: "person"}
	person.AddCommand(newPersonTiersCommand(deps))
	root.AddCommand(person)
	root.SetArgs(append([]string{"person", "tiers"}, args...))
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(ctx)
	return stdout.String(), stderr.String(), err
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
		Signals: persontiers.Signals{
			OutboundLong: 2, InboundLong: 2, ActiveDaysLong: 4,
			ExcludedListEvents: 4, LargeAudienceInboundEvents: 9,
		},
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
}

// The [people.tiers] and [activity] settings reach the store only through bind,
// so this runs the real command on a loaded config and a seeded archive. Mail
// on three days, both ways, is tier 2 by default.
func TestPersonTiersUseTheLoadedConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	markDaemonCLISubprocessForTest(t)
	home := t.TempDir()
	st, err := store.OpenForTest(filepath.Join(home, "msgvault.db"))
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	conversation, err := st.EnsureConversation(source.ID, "tier-thread", "Tier")
	require.NoError(err)
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(err)
	friend, err := st.EnsureParticipant("friend@example.com", "Friend", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(friend)
	require.NoError(err)
	for day, from := range []int64{owner, friend, friend} {
		to := owner + friend - from
		id, err := st.UpsertMessage(&store.Message{
			ConversationID: conversation, SourceID: source.ID, MessageType: "email",
			SourceMessageID: fmt.Sprintf("tier-%d", day),
			SentAt:          sql.NullTime{Time: time.Now().UTC().AddDate(0, 0, -day-1), Valid: true},
			SenderID:        sql.NullInt64{Int64: from, Valid: true},
		})
		require.NoError(err)
		require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{from}, []string{""}))
		require.NoError(st.ReplaceMessageRecipients(id, "to", []int64{to}, []string{""}))
	}
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: 25})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	require.NoError(st.Close())
	preview := func(toml string) (store.PersonTierPreview, string) {
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(os.WriteFile(path, []byte(toml), 0o600))
		cfg, err := config.Load(path, home)
		require.NoError(err)
		ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
		stdout, stderr, err := executePersonTiersCommandContext(ctx, defaultPersonTiersCommandDeps(), "preview", "--json")
		require.NoError(err)
		var out store.PersonTierPreview
		require.NoError(json.Unmarshal([]byte(stdout), &out))
		return out, stderr
	}
	listed, _ := preview("")
	require.Len(listed.People, 1)
	assert.Equal(person.ID, listed.People[0].PersonID)
	assert.Equal(2, listed.People[0].Tier)
	raised, _ := preview("[people.tiers.tier2]\nmin_active_days_long = 4\n")
	assert.Empty(raised.People, "a raised threshold from [people.tiers] drops the person")
	_, warning := preview("[activity]\ntimezone = \"America/New_York\"\n")
	assert.Contains(warning, "settings_changed")

	// The daemon's CLI children load leniently, so a misspelled threshold must
	// stop the preview instead of falling back to the default.
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte("[people.tiers.tier2]\nmin_active_days_lng = 4\n"), 0o600))
	snapshot, err := config.ReadConfigFile(path)
	require.NoError(err)
	lenient, err := config.LoadClientConfigFile(snapshot, home)
	require.NoError(err)
	ctx := testInvocationContext(t.Context(), lenient, invocationOptions{})
	_, _, err = executePersonTiersCommandContext(ctx, defaultPersonTiersCommandDeps(), "preview", "--json")
	require.ErrorIs(err, persontiers.ErrInvalidConfig)
	assert.ErrorContains(err, "min_active_days_lng")
}
