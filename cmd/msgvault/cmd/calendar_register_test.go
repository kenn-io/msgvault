package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/testutil"
)

// The service-account path of add-calendar skips consent entirely and relies
// on registerCalendarsAndReport to enumerate calendars, create the gcal source
// rows, and report them — so that helper is exercised directly with a mock API.
func TestRegisterCalendarsAndReport_RegistersSourcesAndReports(t *testing.T) {
	st := testutil.NewTestStore(t)
	api := gcal.NewMockAPI()
	api.Calendars = []gcal.Calendar{
		{ID: "primary", Summary: "Alice", AccessRole: "owner"},
		{ID: "team@group.calendar.google.com", Summary: "Team", AccessRole: "writer"},
		{ID: "holidays", Summary: "Holidays", AccessRole: "reader"}, // excluded by default role filter
	}

	var out bytes.Buffer
	err := registerCalendarsAndReport(context.Background(), &out, st, api,
		"alice@example.com", "acme", true)
	require := require.New(t)
	assert := assert.New(t)
	require.NoError(err)

	for _, id := range []string{"alice@example.com/primary", "alice@example.com/team@group.calendar.google.com"} {
		src, err := st.GetSourceByIdentifier(id)
		require.NoError(err, id)
		assert.Equal("gcal", src.SourceType)
	}
	_, err = st.GetSourceByIdentifier("alice@example.com/holidays")
	require.Error(err, "reader-only calendar is not registered without --all-calendars")

	assert.Contains(out.String(), "Registered 2 calendar(s) for alice@example.com")
	assert.Contains(out.String(), "Next: ")
}

func TestRegisterCalendarsAndReport_NoMatchIsNotAnError(t *testing.T) {
	st := testutil.NewTestStore(t)
	api := gcal.NewMockAPI()
	api.Calendars = []gcal.Calendar{{ID: "holidays", AccessRole: "reader"}}

	var out bytes.Buffer
	require.NoError(t, registerCalendarsAndReport(context.Background(), &out, st, api,
		"alice@example.com", "", false))
	assert.Contains(t, out.String(), "No calendars matched the filter")
}

// The daemon-side plan step must not demand client_secrets for a named app
// configured with a service account: it resolves the binding and reports that
// no consent/escalation round trip is needed.
func TestPlanCLIAddCalendar_ServiceAccountAppNeedsNoConsent(t *testing.T) {
	st := testutil.NewTestStore(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{
		HomeDir: t.TempDir(),
		OAuth: config.OAuthConfig{
			Apps: map[string]config.OAuthApp{
				"sa": {ServiceAccountKey: "/nonexistent/service-account.json"},
			},
		},
	}

	plan, err := planCLIAddCalendar(context.Background(), st, api.CLIAddCalendarPlanRequest{
		Email: "bob@example.com", OAuthApp: "sa", OAuthAppExplicit: true,
	})
	require.NoError(t, err, "service-account apps have no client_secrets and must not be asked for one")
	assert := assert.New(t)
	assert.Equal("sa", plan.OAuthApp)
	assert.True(plan.OAuthAppResolved)
	assert.False(plan.NeedsScopeEscalation)
}
