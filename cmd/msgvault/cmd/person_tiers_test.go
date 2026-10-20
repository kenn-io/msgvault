package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// tiersStore serves a canned preview and explanation and records the arguments
// it was called with.
type tiersStore struct {
	preview     store.PersonTierPreview
	explanation *store.PersonTierExplanation
	tier        int
	personID    int64
	options     store.PersonTierOptions
}

func (s *tiersStore) PreviewPersonTiersContext(_ context.Context, options store.PersonTierOptions, tier int) (*store.PersonTierPreview, error) {
	s.tier, s.options = tier, options
	return &s.preview, nil
}

func (s *tiersStore) ExplainPersonTierContext(_ context.Context, personID int64, options store.PersonTierOptions) (*store.PersonTierExplanation, error) {
	s.personID, s.options = personID, options
	return s.explanation, nil
}

func tierRow(id int64, name string, tier int, tracked bool) store.PersonTierRow {
	return store.PersonTierRow{PersonID: id, DisplayName: &name, Tier: tier, Rule: fmt.Sprintf("tier%d_rule", tier), Tracked: tracked}
}

// readyPreview lists rows in the order the store returns them: tier first,
// then newest counted day, so the IDs here are deliberately not ascending.
func readyPreview(rows ...store.PersonTierRow) store.PersonTierPreview {
	return store.PersonTierPreview{
		AsOf: "2026-10-17", Readiness: store.PersonTierReadiness{Ready: true, Reasons: []string{}}, People: rows,
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
	stdout, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview", "--json", "--tier", "2")
	require.NoError(err)
	var out store.PersonTierPreview
	require.NoError(json.Unmarshal([]byte(stdout), &out))
	require.Len(out.People, 2)
	assert.Equal(int64(3), out.People[0].PersonID)
	assert.True(out.People[1].Tracked)
	assert.Equal("2026-10-17", out.AsOf)
	assert.Equal(2, st.tier)
	assert.Equal(tiersTestOptions, st.options, "the configured rules and [activity] settings reach the store")
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
	assert.Regexp(`1\s+3\s+Cleo\s+tier1_rule\s+false\s+2026-10-16`, stdout)
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

func TestPersonTiersPreviewRejectsAnUnknownTier(t *testing.T) {
	st := &tiersStore{}
	_, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview", "--tier", "3")
	require.ErrorContains(t, err, "--tier must be 1 or 2")
	assert.Zero(t, st.tier)
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
	stdout, _, err := executePersonTiersCommand(t, deps, "preview", "--tier", "1", "--json")
	require.NoError(err)
	assert.Equal(t, "forwarded\n", stdout)
	_, _, err = executePersonTiersCommand(t, deps, "explain", "7")
	require.NoError(err)
	assert.Equal(t, [][]string{
		{"person", "tiers", "preview", "--json", "--tier=1"},
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
	assert.Equal(tiersTestOptions, st.options)
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

// The daemon side end to end: a real store seeded through the real projector,
// preview's JSON ID accepted by explain.
func TestPersonTiersWorkflowAgainstARealStore(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	friend := f.EnsureParticipant("friend@example.com", "Friend", "example.com")
	person, _, err := f.Store.CreatePersonFromParticipant(friend)
	require.NoError(err)
	now := time.Now().UTC()
	for i := range 8 {
		from, to := friend, owner
		if i%2 == 0 {
			from, to = owner, friend
		}
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("workflow-%d", i)).WithSentAt(now.AddDate(0, 0, -i-1)).Build()
		message.SenderID = sql.NullInt64{Int64: from, Valid: true}
		id, err := f.Store.UpsertMessage(message)
		require.NoError(err)
		require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{to}, nil))
	}
	projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 50, MaxDirectCounterparts: 25})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	deps := daemonTiersDeps(f.Store)

	stdout, stderr, err := executePersonTiersCommand(t, deps, "preview", "--json", "--tier", "1")
	require.NoError(err)
	assert.Empty(stderr)
	var preview store.PersonTierPreview
	require.NoError(json.Unmarshal([]byte(stdout), &preview))
	require.Len(preview.People, 1)
	assert.Equal(person.ID, preview.People[0].PersonID)
	assert.Equal("tier1_intensity", preview.People[0].Rule)

	stdout, _, err = executePersonTiersCommand(t, deps, "explain", strconv.FormatInt(preview.People[0].PersonID, 10), "--json")
	require.NoError(err)
	var explanation store.PersonTierExplanation
	require.NoError(json.Unmarshal([]byte(stdout), &explanation))
	assert.Equal(1, explanation.Tier)
	assert.Equal(4, explanation.Signals.OutboundShort)
	assert.Equal(4, explanation.Signals.InboundShort)
}
