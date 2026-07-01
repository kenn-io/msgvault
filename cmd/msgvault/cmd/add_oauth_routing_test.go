package cmd

import (
	"bytes"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestAddAccountUsesDaemonRunner(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"add-account",
			"--display-name=Work",
			"--headless",
			"--no-default-identity",
			"--oauth-app=acme",
			"alice@example.com",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Account authorized\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := newAddAccountCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"alice@example.com",
		"--display-name", "Work",
		"--headless",
		"--no-default-identity",
		"--oauth-app", "acme",
	})

	require.NoError(cmd.Execute(), "add-account")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Equal("Account authorized\n", stdout.String(), "stdout")
}

func TestAddO365UsesDaemonRunner(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"add-o365",
			"--no-default-identity",
			"--tenant=acme",
			"alice@example.com",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Microsoft 365 account added\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := newAddO365Cmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"alice@example.com", "--tenant", "acme", "--no-default-identity"})

	require.NoError(cmd.Execute(), "add-o365")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Equal("Microsoft 365 account added\n", stdout.String(), "stdout")
}

func TestAddTeamsUsesDaemonRunner(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"add-teams",
			"--no-default-identity",
			"--tenant=acme",
			"alice@example.com",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Microsoft Teams account authorized\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := newAddTeamsCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"alice@example.com", "--tenant", "acme", "--no-default-identity"})

	require.NoError(cmd.Execute(), "add-teams")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Equal("Microsoft Teams account authorized\n", stdout.String(), "stdout")
}

func TestAddCalendarUsesDaemonRunner(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	saveCalendarGlobals := saveCalendarCommandGlobals()
	t.Cleanup(saveCalendarGlobals)

	server, runRequests, planRequests := newDaemonCLIAddCalendarTestServer(t, func(req daemonCLIAddCalendarPlanTestRequest) {
		assert.Equal("alice@example.com", req.Email, "plan email")
		assert.Equal("acme", req.OAuthApp, "plan oauth app")
		assert.True(req.OAuthAppExplicit, "plan oauth app explicit")
		assert.True(req.Headless, "plan headless")
	}, nil, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"add-calendar",
			"--all-calendars",
			"--calendars=primary",
			"--calendars=team@example.com",
			"--headless",
			"--min-access-role=reader",
			"--oauth-app=acme",
			"alice@example.com",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Registered 2 calendars\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := newAddCalendarCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"alice@example.com",
		"--all-calendars",
		"--calendars", "primary,team@example.com",
		"--headless",
		"--min-access-role", "reader",
		"--oauth-app", "acme",
	})

	require.NoError(cmd.Execute(), "add-calendar")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Equal("Registered 2 calendars\n", stdout.String(), "stdout")
}

func TestSyncCalendarUsesDaemonRunner(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	saveCalendarGlobals := saveCalendarCommandGlobals()
	t.Cleanup(saveCalendarGlobals)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"sync-calendar",
			"--after=2024-01-01",
			"--all-calendars",
			"--before=2024-02-01",
			"--calendar=primary",
			"--calendar=team@example.com",
			"--full",
			"--limit=10",
			"--min-access-role=writer",
			"--noresume",
			"--oauth-app=acme",
			"alice@example.com",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Calendar sync complete\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := newSyncCalendarCmd()
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{
		"alice@example.com",
		"--after", "2024-01-01",
		"--all-calendars",
		"--before", "2024-02-01",
		"--calendar", "primary",
		"--calendar", "team@example.com",
		"--full",
		"--limit", "10",
		"--min-access-role", "writer",
		"--noresume",
		"--oauth-app", "acme",
	})

	require.NoError(cmd.Execute(), "sync-calendar")
	assert.Equal(1, int(requests.Load()), "runner endpoint calls")
	assert.Equal("Calendar sync complete\n", stdout.String(), "stdout")
}

func TestAddCalendarPromptsScopeEscalationBeforeDaemonRunner(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	saveCalendarGlobals := saveCalendarCommandGlobals()
	t.Cleanup(saveCalendarGlobals)

	server, runRequests, planRequests := newDaemonCLIAddCalendarTestServer(t, func(req daemonCLIAddCalendarPlanTestRequest) {
		assert.Equal("alice@example.com", req.Email, "plan email")
	}, map[string]any{
		"needs_scope_escalation": true,
		"headline":               "CALENDAR ACCESS REQUIRED",
		"body_lines": []string{
			"Calendar sync needs read-only Calendar access.",
			"",
			"Re-authorizing REPLACES the granted scopes.",
		},
		"cancel_hint": "Cancelled. Calendar was not added.",
	}, func(req daemonCLIRunTestRequest) {
		assert.Equal([]string{
			"add-calendar",
			"--scope-escalation-confirmed",
			"alice@example.com",
		}, req.Args, "args")
	}, `{"type":"stdout","data":"Registered 1 calendar\n"}`, `{"type":"complete"}`)
	configureRemoteDaemonForTest(t, server.URL)

	var stdout bytes.Buffer
	cmd := newAddCalendarCmd()
	cmd.SetIn(bytes.NewBufferString("y\n"))
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"alice@example.com"})

	require.NoError(cmd.Execute(), "add-calendar")
	assert.Equal(1, int(planRequests.Load()), "plan endpoint calls")
	assert.Equal(1, int(runRequests.Load()), "runner endpoint calls")
	assert.Contains(stdout.String(), "CALENDAR ACCESS REQUIRED", "frontend prompt")
	assert.Contains(stdout.String(), "Registered 1 calendar", "daemon stdout")
}

func saveCalendarCommandGlobals() func() {
	saved := struct {
		addOAuthApp   string
		addHeadless   bool
		addAll        bool
		addMinRole    string
		addCalendars  []string
		syncOAuthApp  string
		syncFull      bool
		syncLimit     int
		syncAfter     string
		syncBefore    string
		syncNoResume  bool
		syncAll       bool
		syncMinRole   string
		syncCalendars []string
	}{
		addOAuthApp:   calAddOAuthApp,
		addHeadless:   calAddHeadless,
		addAll:        calAddAll,
		addMinRole:    calAddMinRole,
		addCalendars:  append([]string(nil), calAddCalendars...),
		syncOAuthApp:  calSyncOAuthApp,
		syncFull:      calSyncFull,
		syncLimit:     calSyncLimit,
		syncAfter:     calSyncAfter,
		syncBefore:    calSyncBefore,
		syncNoResume:  calSyncNoResume,
		syncAll:       calSyncAll,
		syncMinRole:   calSyncMinRole,
		syncCalendars: append([]string(nil), calSyncCalendars...),
	}
	return func() {
		calAddOAuthApp = saved.addOAuthApp
		calAddHeadless = saved.addHeadless
		calAddAll = saved.addAll
		calAddMinRole = saved.addMinRole
		calAddCalendars = saved.addCalendars
		calSyncOAuthApp = saved.syncOAuthApp
		calSyncFull = saved.syncFull
		calSyncLimit = saved.syncLimit
		calSyncAfter = saved.syncAfter
		calSyncBefore = saved.syncBefore
		calSyncNoResume = saved.syncNoResume
		calSyncAll = saved.syncAll
		calSyncMinRole = saved.syncMinRole
		calSyncCalendars = saved.syncCalendars
	}
}
