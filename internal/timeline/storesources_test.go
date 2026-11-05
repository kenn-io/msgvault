package timeline

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFreshnessSourcesOnlyIncludeIssues(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, status string
		errors                int
		disabled, stale, keep bool
		caldav                bool
	}{
		{name: "current"},
		{name: "caldav never synced", caldav: true, status: "never_synced", keep: true},
		{name: "stale", stale: true, status: "stale", keep: true},
		{name: "failed", outcome: "failed", status: "current", keep: true},
		{name: "cancelled", outcome: "cancelled", status: "current", keep: true},
		{name: "paused", outcome: "paused", status: "current", keep: true},
		{name: "partial", errors: 2, status: "current", keep: true},
		{name: "disabled", disabled: true, status: "current", keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", "owner@example.test")
			requirements.NoError(err)
			run, err := st.StartSync(source.ID, "incremental")
			requirements.NoError(err)
			requirements.NoError(st.CompleteSync(run, "cursor"))
			_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET errors_count = ? WHERE id = ?"), tc.errors, run)
			requirements.NoError(err)
			if tc.outcome != "" {
				run, err = st.StartSync(source.ID, "incremental")
				requirements.NoError(err)
				_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET status = ? WHERE id = ?"), tc.outcome, run)
				requirements.NoError(err)
			}
			cfg := config.NewDefaultConfig()
			cfg.Accounts = []config.AccountSchedule{{Email: "owner@example.test", Enabled: !tc.disabled}}
			now := time.Now().UTC()
			if tc.stale {
				now = now.Add(48 * time.Hour)
			}
			if tc.caldav {
				cfg.Accounts = nil
				cfg.CalDAV.Sources = []config.CalDAVSource{{Name: "personal", URL: "https://calendar.example.com/dav/", Username: "owner@example.com"}}
				_, err := st.GetOrCreateSource("caldav", `["personal","https://calendar.example.com/dav/work/"]`)
				requirements.NoError(err)
			}
			src, err := NewStoreSources(st, cfg)
			requirements.NoError(err)
			report, err := src.Inventory.Build(t.Context(), now)
			requirements.NoError(err)
			assertions.Equal(now, report.AsOf)
			if tc.keep {
				requirements.Len(report.Sources, 1)
				assertions.Equal(tc.status, report.Sources[0].Status)
				if tc.caldav {
					assertions.Equal("msgvault sync-caldav personal", report.Sources[0].SyncCommand)
				} else {
					assertions.Equal("msgvault sync-incremental owner@example.test", report.Sources[0].SyncCommand)
					assertions.Contains(Markdown(&Timeline{Freshness: report}), "gmail:owner@example.test")
				}
			} else {
				assertions.Empty(report.Sources)
			}
		})
	}
}

func TestStoreSourcesDefaultLoadedTimezoneHint(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, tc := range []struct {
		name, body, zone string
		hint             bool
	}{
		{"omitted", "", "UTC", true},
		{"explicit UTC", "[activity]\ntimezone = \"UTC\"\n", "UTC", true},
		{"configured IANA", "[activity]\ntimezone = \"America/Vancouver\"\n", "America/Vancouver", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tc.body), 0o600))
			cfg, err := config.Load(path, "")
			requirements.NoError(err)
			src, err := NewStoreSources(st, cfg)
			requirements.NoError(err)
			assertions.Equal(tc.zone, src.Location.String())
			assertions.Equal(tc.hint, src.ZoneDefault)
		})
	}
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	require.NoError(t, err)
	assert.True(t, src.ZoneDefault)
}

// Stored projection policy governs full-audience reads.
func TestStoreSourcesUsesStoredProjectionPolicy(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	sources, err := NewStoreSources(st, config.NewDefaultConfig())
	requirements.NoError(err)
	sources.Clock = func() time.Time { return now }
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	requirements.NoError(err)
	other, err := st.EnsureParticipant("other@example.com", "Other", "example.com")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	conversation, err := st.EnsureConversation(src.ID, "policy-audience", "Policy audience")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conversation, SourceMessageID: "policy-audience", MessageType: "email", SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}, SenderID: sql.NullInt64{Int64: peer, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
	requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{owner, other}, nil))
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", MaxDirectCounterparts: 1})
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)
	for _, includeNoise := range []bool{false, true} {
		req := dayRequest()
		req.Date, req.IncludeNoise = "2027-03-01", includeNoise
		got, err := Build(t.Context(), sources, req)
		requirements.NoError(err)
		var bursts []Entry
		for _, day := range got.Days {
			bursts = append(bursts, day.Bursts...)
		}
		if includeNoise {
			requirements.Len(bursts, 1)
			requirements.NotNil(bursts[0].Burst)
			assertions.Zero(bursts[0].Burst.Inbound)
			assertions.Zero(bursts[0].Burst.Outbound)
		} else {
			assertions.Empty(bursts)
		}
	}
}
