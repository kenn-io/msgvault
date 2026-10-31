package api

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestCLIXRemovePostStrictModes(t *testing.T) {
	for _, id := range []string{"500", "9007199254740993", strings.Repeat("9", 128)} {
		for _, args := range [][]string{
			{"sync-x", "--source-id=42", "--remove-post=" + id},
			{"sync-x", "--remove-post", id, "--source-id", "42", "--json"},
			{"sync-x", "--source-id=42", "--remove-post=" + id, "--json=false"},
		} {
			got, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
			require.NoError(t, err)
			assert.Equal(t, int64(42), got.SourceID)
			assert.Equal(t, id, got.RemovePost)
			assert.False(t, got.Archive)
			assert.False(t, got.AcknowledgeReadModel)
			assert.Nil(t, got.MaxSpendMicros)
			assert.True(t, cliRunCommandAllowed(args))
		}
	}

	base := []string{"sync-x", "--source-id=42", "--remove-post=500"}
	for _, flag := range []string{
		"--after=2020-01-01", "--archive", "--archive=false",
		"--max-spend=0", "--job-id=xjob_example", "--new-job", "--new-job=false",
		"--build-cache", "--build-cache=false", "--no-build-cache", "--no-build-cache=false",
		"--acknowledge-read-model", "--acknowledge-read-model=false",
		"--remove-post=501", "--token-file=/tmp/synthetic", "extra",
	} {
		args := append(append([]string{}, base...), flag)
		_, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
		require.Error(t, err, "%v", args)
		assert.False(t, cliRunCommandAllowed(args), "%v", args)
	}
	for _, id := range []string{"", "0", "01", "-1", "+1", " 1", "1 ", "1.0", "1e3", "１２", strings.Repeat("9", 129)} {
		args := []string{"sync-x", "--source-id=42", "--remove-post=" + id}
		_, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
		require.Error(t, err, "%q", id)
		assert.False(t, cliRunCommandAllowed(args), "%q", id)
	}
	for _, args := range [][]string{
		{"sync-x", "--remove-post=500"},
		{"sync-x", "--source-id=01", "--remove-post=500"},
		{"sync-x", "--source-id=42", "--remove-post"},
		{"sync-x", "--source-id=42", "--remove-post=500", "--", "extra"},
	} {
		_, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
		require.Error(t, err, "%v", args)
		assert.False(t, cliRunCommandAllowed(args), "%v", args)
	}
	for _, req := range []CLIRunRequest{
		{Args: base, Cwd: "/tmp"},
		{Args: base, Env: map[string]string{"MSGVAULT_X_CLIENT_SECRET": "synthetic"}},
		{Args: base, Grant: &agentgrant.Grant{}},
		{Args: base, GrantDecided: true},
	} {
		_, err := ParseCLIRunXSync(req)
		require.Error(t, err)
	}
}

func TestCLIXExportRemovalsStrictModes(t *testing.T) {
	for _, args := range [][]string{
		{"sync-x", "--export-removals"},
		{"sync-x", "--export-removals=true", "--json"},
		{"sync-x", "--json=false", "--export-removals"},
	} {
		got, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
		require.NoError(t, err)
		assert.True(t, got.ExportRemovals)
		assert.Zero(t, got.SourceID, "the removal record covers the whole archive")
		assert.Empty(t, got.RemovePost)
		assert.False(t, got.Archive)
		assert.False(t, got.AcknowledgeReadModel)
		assert.True(t, cliRunCommandAllowed(args))
	}
	base := []string{"sync-x", "--export-removals"}
	for _, extra := range []string{
		"--source-id=42", "--after=2020-01-01", "--archive", "--archive=false",
		"--max-spend=0", "--job-id=xjob_example", "--new-job", "--new-job=false",
		"--build-cache", "--build-cache=false", "--no-build-cache", "--no-build-cache=false",
		"--remove-post=500", "--acknowledge-read-model", "--acknowledge-read-model=false",
		"--export-removals", "--token-file=/tmp/synthetic", "extra",
	} {
		args := append(append([]string{}, base...), extra)
		_, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
		require.Error(t, err, "%v", args)
		assert.False(t, cliRunCommandAllowed(args), "%v", args)
	}
	for _, value := range []string{"false", "0", "maybe", ""} {
		args := []string{"sync-x", "--export-removals=" + value}
		_, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
		require.Error(t, err)
		assert.False(t, cliRunCommandAllowed(args))
	}
	for _, req := range []CLIRunRequest{
		{Args: base, Cwd: "/tmp"},
		{Args: base, Env: map[string]string{"MSGVAULT_X_CLIENT_SECRET": "synthetic"}},
		{Args: base, Grant: &agentgrant.Grant{}},
		{Args: base, GrantDecided: true},
	} {
		_, err := ParseCLIRunXSync(req)
		require.Error(t, err)
	}
}

func TestCLIXRegisterStrictArguments(t *testing.T) {
	for _, args := range [][]string{{"add-x", "--register", "--user-id", "1001"}, {"add-x", "--user-id=1001", "--register=true", "--json"}} {
		request, err := ParseCLIRunXRegister(CLIRunRequest{Args: args})
		require.NoError(t, err)
		assert.Equal(t, "1001", request.UserID)
		assert.True(t, cliRunCommandAllowed(args))
	}
	for _, args := range [][]string{{"add-x"}, {"add-x", "--register=false", "--user-id", "1001"}, {"add-x", "--register", "--user-id", "01"}, {"add-x", "--register", "--user-id", "1001", "--user-id", "1002"}, {"add-x", "--register", "--user-id", "1001", "--token-file", "secret"}, {"add-x", "--client-id", "example"}, {"add-x", "--register", "--user-id", "1001", "position"}} {
		_, err := ParseCLIRunXRegister(CLIRunRequest{Args: args})
		require.Error(t, err)
		assert.False(t, cliRunCommandAllowed(args))
	}
	valid := []string{"add-x", "--register", "--user-id", "1001"}
	for _, request := range []CLIRunRequest{{Args: valid, Cwd: "/tmp"}, {Args: valid, Env: map[string]string{"MSGVAULT_X_CLIENT_SECRET": "secret"}}, {Args: valid, Grant: &agentgrant.Grant{}}, {Args: valid, GrantDecided: true}} {
		_, err := ParseCLIRunXRegister(request)
		require.Error(t, err)
	}
}

func TestCLIXSyncStrictArguments(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		after      string
		ack, cache bool
	}{
		{args: []string{"sync-x", "--source-id", "42", "--json"}},
		{args: []string{"sync-x", "--source-id=42", "--after=2026-10-01", "--build-cache=true"}, after: "2026-10-01T00:00:00Z", cache: true},
		{args: []string{"sync-x", "--source-id=42", "--after", "2026-10-01T02:00:00+02:00"}, after: "2026-10-01T00:00:00Z"},
		{args: []string{"sync-x", "--source-id=42", "--acknowledge-read-model", "--json=false"}, ack: true},
	} {
		got, err := ParseCLIRunXSync(CLIRunRequest{Args: tc.args})
		require.NoError(t, err)
		assert.Equal(t, int64(42), got.SourceID)
		assert.Equal(t, tc.ack, got.AcknowledgeReadModel)
		assert.Equal(t, tc.cache, got.BuildCache)
		if tc.after != "" {
			assert.Equal(t, tc.after, got.After.Format(time.RFC3339))
		}
		assert.True(t, cliRunCommandAllowed(tc.args))
	}
	invalid := [][]string{
		{"sync-x"}, {"sync-x", "42"}, {"sync-x", "--source-id=0"}, {"sync-x", "--source-id=-1"}, {"sync-x", "--source-id=01"}, {"sync-x", "--source-id=9223372036854775808"},
		{"sync-x", "--source-id=42", "--source-id=43"}, {"sync-x", "--source-id=42", "--json", "--json"}, {"sync-x", "--source-id=42", "--after="}, {"sync-x", "--source-id=42", "--after=tomorrow"},
		{"sync-x", "--source-id=42", "--json=maybe"}, {"sync-x", "--source-id=42", "--unknown"}, {"sync-x", "--source-id=42", "--archive"}, {"sync-x", "--source-id=42", "--refresh"}, {"sync-x", "--source-id=42", "--build-cache", "--no-build-cache"},
		{"sync-x", "--source-id=42", "--acknowledge-read-model=false"}, {"sync-x", "--source-id=42", "--acknowledge-read-model", "--after=2026-10-01"},
		{"sync-x", "--source-id=42", "--acknowledge-read-model", "--build-cache=false"}, {"sync-x", "--source-id=42", "--acknowledge-read-model", "--no-build-cache"},
		{"sync-x", "--source-id=42", "--token-file=/tmp/example"}, {"sync-x", "--source-id=42", "--grant-decided"}, {"sync-x", "--source-id=42", "--", "extra"},
	}
	for _, args := range invalid {
		_, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
		require.Error(t, err, "%v", args)
		assert.False(t, cliRunCommandAllowed(args), "%v", args)
	}
	valid := []string{"sync-x", "--source-id=42"}
	for _, req := range []CLIRunRequest{{Args: valid, Cwd: "/tmp"}, {Args: valid, Env: map[string]string{"MSGVAULT_DISCORD_TOKEN": "synthetic"}}, {Args: valid, Grant: &agentgrant.Grant{}}, {Args: valid, GrantDecided: true}} {
		_, err := ParseCLIRunXSync(req)
		require.Error(t, err)
	}
}

func TestCLIXArchiveStrictModesAndMoney(t *testing.T) {
	base := []string{"sync-x", "--source-id=42", "--archive", "--after=2020-01-01"}
	for _, tc := range []struct {
		extra []string
		cap   *int64
		job   string
		fresh bool
	}{
		{}, {extra: []string{"--max-spend=0"}, cap: new(int64(0))}, {extra: []string{"--max-spend", "25.000001"}, cap: new(int64(25_000_001))},
		{extra: []string{"--max-spend=1e-6", "--job-id=xjob_example"}, cap: new(int64(1)), job: "xjob_example"},
		{extra: []string{"--max-spend=+0.15", "--new-job"}, cap: new(int64(150_000)), fresh: true},
		{extra: []string{"--job-id=xjob_example"}, job: "xjob_example"},
	} {
		args := append(append([]string{}, base...), tc.extra...)
		got, err := ParseCLIRunXSync(CLIRunRequest{Args: args})
		require.NoError(t, err)
		assert.True(t, got.Archive)
		assert.Equal(t, tc.cap, got.MaxSpendMicros)
		assert.Equal(t, tc.job, got.JobID)
		assert.Equal(t, tc.fresh, got.NewJob)
		assert.True(t, cliRunCommandAllowed(args))
	}
	invalid := [][]string{
		{"--source-id=42", "--archive"}, {"--source-id=42", "--max-spend=0"}, {"--source-id=42", "--job-id=xjob_example"}, {"--source-id=42", "--new-job=false"},
		{"--source-id=42", "--archive=false", "--after=2020-01-01", "--max-spend=1"},
	}
	for _, extra := range [][]string{{"--max-spend=-0"}, {"--max-spend=NaN"}, {"--max-spend=Inf"}, {"--max-spend=0.0000001"}, {"--max-spend=9223372036854.775808"}, {"--max-spend="}, {"--max-spend=1", "--max-spend=2"}, {"--job-id="}, {"--job-id=xjob_example", "--new-job=false"}, {"--archive=false"}, {"--new-job=maybe"}, {"--remove-post=500"}, {"--refresh"}} {
		invalid = append(invalid, append(append([]string{}, base[1:]...), extra...))
	}
	for _, args := range invalid {
		full := append([]string{"sync-x"}, args...)
		_, err := ParseCLIRunXSync(CLIRunRequest{Args: full})
		require.Error(t, err, "%v", full)
		assert.False(t, cliRunCommandAllowed(full))
	}
	for _, flag := range []string{"--archive=false", "--new-job=false", "--max-spend=0", "--job-id=xjob_example"} {
		_, err := ParseCLIRunXSync(CLIRunRequest{Args: []string{"sync-x", "--source-id=42", "--acknowledge-read-model", flag}})
		require.Error(t, err, flag)
	}
}
