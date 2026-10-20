package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

// tiersStore serves a canned preview and explanation.
type tiersStore struct {
	preview     store.PersonTierPreview
	explanation *store.PersonTierExplanation
	tierFilter  int
}

func (s *tiersStore) PreviewPersonTiersContext(_ context.Context, _ store.PersonTierOptions, tier int) (*store.PersonTierPreview, error) {
	s.tierFilter = tier
	return &s.preview, nil
}

func (s *tiersStore) ExplainPersonTierContext(context.Context, int64, store.PersonTierOptions) (*store.PersonTierExplanation, error) {
	return s.explanation, nil
}

func tierRow(id int64, name string, tier int, tracked bool) store.PersonTierRow {
	return store.PersonTierRow{PersonID: id, DisplayName: &name, Tier: tier, Rule: fmt.Sprintf("tier%d_rule", tier), Tracked: tracked}
}

// readyPreview lists rows in the order the store returns them: tier first,
// then most recent contact, so the IDs here are deliberately not ascending.
func readyPreview(rows ...store.PersonTierRow) store.PersonTierPreview {
	return store.PersonTierPreview{
		AsOf: "2026-10-17", Readiness: store.PersonTierReadiness{Ready: true, Reasons: []string{}}, People: rows,
	}
}

func daemonTiersDeps(st personTiersCommandStore) personTiersCommandDeps {
	return personTiersCommandDeps{
		options: func() (store.PersonTierOptions, error) {
			return store.PersonTierOptions{Config: persontiers.DefaultConfig()}, nil
		},
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
	assert.Equal(2, st.tierFilter)
}

func TestPersonTiersPreviewPrintsATable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &tiersStore{preview: readyPreview(tierRow(3, "Cleo", 1, false))}
	stdout, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview")
	require.NoError(err)
	assert.Regexp(`1\s+3\s+Cleo\s+tier1_rule\s+false\s+-`, stdout)
}

func TestPersonTiersPreviewWarnsWhenProjectionIsBehind(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := &tiersStore{preview: readyPreview(tierRow(3, "Cleo", 1, false))}
	st.preview.Readiness = store.PersonTierReadiness{Reasons: []string{"watermark_behind"}}
	stdout, stderr, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview", "--json")
	require.NoError(err)
	assert.Contains(stderr, "watermark_behind")
	assert.True(json.Valid([]byte(stdout)), "stdout stays JSON")
}

func TestPersonTiersPreviewRejectsBadAndRemovedFlags(t *testing.T) {
	require := require.New(t)
	st := &tiersStore{}
	for _, args := range [][]string{{"preview", "--tier", "3"}, {"preview", "--track"}, {"preview", "--max", "5"}, {"preview", "--yes"}} {
		_, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), args...)
		require.Error(err, args)
	}
	assert.Zero(t, st.tierFilter)
}

func TestPersonTiersPreviewNamesTheProjectionSettings(t *testing.T) {
	require := require.New(t)
	st := &tiersStore{preview: readyPreview(tierRow(3, "Cleo", 1, false))}
	st.preview.Projection = store.PersonTierProjection{Timezone: "America/New_York", MaxDirect: 25}
	stdout, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview")
	require.NoError(err)
	assert.Contains(t, stdout, "counts built with timezone America/New_York, direct limit 25")
}

func TestPersonTiersWarnOncePerCondition(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := &tiersStore{preview: readyPreview(tierRow(3, "Cleo", 1, false))}
	st.preview.Readiness = store.PersonTierReadiness{Reasons: []string{"watermark_behind"}}
	st.preview.Projection = store.PersonTierProjection{Timezone: "UTC", MaxDirect: 25}
	st.preview.Provisional = true
	_, stderr, err := executePersonTiersCommand(t, daemonTiersDeps(st), "preview")
	require.NoError(err)
	assert.Equal(1, strings.Count(stderr, "watermark_behind"))
	assert.Equal(1, strings.Count(stderr, "counts follow timezone UTC and direct limit 25"))
	assert.Equal(2, strings.Count(stderr, "Warning:"))
}

func TestPersonTiersPreviewForwardsTheCommandFromTheCLI(t *testing.T) {
	require := require.New(t)
	var forwarded []string
	deps := personTiersCommandDeps{
		isDaemonSubprocess: func() bool { return false },
		proxy: func(_ *cobra.Command, runArgs []string, out io.Writer) error {
			forwarded = runArgs
			_, err := io.WriteString(out, "forwarded\n")
			return err
		},
	}
	stdout, _, err := executePersonTiersCommand(t, deps, "preview", "--tier", "1", "--json")
	require.NoError(err)
	assert.Equal(t, "forwarded\n", stdout)
	assert.Equal(t, []string{"person", "tiers", "preview", "--json", "--tier=1"}, forwarded)
}

func TestPersonTiersExplain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	name := "Seven"
	st := &tiersStore{explanation: &store.PersonTierExplanation{
		PersonID: 7, DisplayName: &name, Tier: 2, Rule: "tier2_two_way", Tracked: true, AsOf: "2026-10-17",
		Projection: store.PersonTierProjection{Timezone: "UTC", MaxDirect: 25},
		Signals:    persontiers.Signals{ExcludedListEvents: 4, LargeAudienceInboundEvents: 9},
		Thresholds: []persontiers.ThresholdCheck{
			{Name: "tier2.min_active_days_long", Observed: 4, Required: 3, Enabled: true, Met: true},
			{Name: "tier2.min_meeting_days_long", Observed: 0, Required: 0},
		},
	}}
	stdout, _, err := executePersonTiersCommand(t, daemonTiersDeps(st), "explain", "7")
	require.NoError(err)
	assert.Contains(stdout, "Tier: 2")
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
	assert := assert.New(t)
	require := require.New(t)
	called := false
	deps := personTiersCommandDeps{
		isDaemonSubprocess: func() bool { return false },
		proxy:              func(*cobra.Command, []string, io.Writer) error { called = true; return nil },
	}
	_, _, err := executePersonTiersCommand(t, deps, "explain", "0")
	require.ErrorContains(err, "positive")
	assert.False(called)
}
