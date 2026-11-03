package cmd

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meta"
)

func groupedCommentCLI(t *testing.T, b commentActionBackend, args ...string) (string, string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newCommentGroup(b))
	var out, diagnostic bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&diagnostic)
	root.SetArgs(append([]string{"comment"}, args...))
	err := executeRootContext(t.Context(), root, append([]string{"comment"}, args...))
	return out.String(), diagnostic.String(), err
}

func TestCommentGroupedPathsAndDiagnostics(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, tc := range []struct {
		args []string
		exit int
		code string
	}{
		{[]string{"typo", "--json"}, 2, "usage"},
		{[]string{"moderate", "--json"}, 2, "usage"},
		{[]string{"moderate", "0", "--hide", "--json"}, 3, "validation"},
		{[]string{"moderate", "7", "--typo", "--json"}, 2, "usage"},
		{[]string{"queue", "--limit", "bad", "--json"}, 3, "validation"},
		{[]string{"reply", "7", "--body", "Example reply", "--json"}, 2, "usage"},
		{[]string{"reply", "7", "--visibility", "invalid", "--body", "Example reply", "--json"}, 3, "validation"},
		{[]string{"reply", "7", "--visibility", "public", "--json"}, 2, "usage"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			b := &cliCommentBackend{}
			out, stderr, err := groupedCommentCLI(t, b, tc.args...)
			require.Error(err)
			assert.Empty(out)
			var coded interface{ ExitCode() int }
			require.ErrorAs(err, &coded)
			assert.Equal(tc.exit, coded.ExitCode())
			var diagnostic struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Hint    string `json:"hint"`
					Exit    int    `json:"exit_code"`
				} `json:"error"`
			}
			require.NoError(json.Unmarshal([]byte(stderr), &diagnostic))
			assert.Equal(tc.code, diagnostic.Error.Code)
			assert.Equal(tc.exit, diagnostic.Error.Exit)
			assert.NotEmpty(diagnostic.Error.Hint)
			assert.Zero(b.calls)
		})
	}
	b := &cliCommentBackend{}
	out, stderr, err := groupedCommentCLI(t, b, "moderate", "7", "--hide", "--dry-run")
	requirements.NoError(err)
	assertions.Contains(out, "binding")
	assertions.Empty(stderr)
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newCommentGroup(b))
	for _, name := range []string{"reply", "moderate", "triage", "queue", "actions"} {
		leaf, rest, err := root.Find([]string{"comment", name})
		requirements.NoError(err)
		assertions.Empty(rest)
		assertions.Equal("msgvault comment "+name, leaf.CommandPath())
		assertions.Empty(leaf.Aliases)
		_, _, err = root.Find([]string{"comment-" + name})
		requirements.Error(err)
	}
}

func TestCommentConflictDiagnosticKeepsPartialReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b := &cliCommentBackend{fail: true}
	out, stderr, err := groupedCommentCLI(t, b, "moderate", "7", "--hide", "--json")
	require.Error(err)
	assert.Empty(out)
	var diagnostic struct {
		Error struct {
			Code    string             `json:"code"`
			Message string             `json:"message"`
			Hint    string             `json:"hint"`
			Exit    int                `json:"exit_code"`
			Details *meta.ActionResult `json:"details"`
		} `json:"error"`
	}
	require.NoError(json.Unmarshal([]byte(stderr), &diagnostic))
	assert.Equal("outcome_unknown", diagnostic.Error.Code)
	assert.Equal(6, diagnostic.Error.Exit)
	require.NotNil(diagnostic.Error.Details)
	require.NotNil(diagnostic.Error.Details.Receipt)
	assert.Equal(int64(3), diagnostic.Error.Details.Receipt.ID)
	var code interface{ ExitCode() int }
	require.ErrorAs(err, &code)
	assert.Equal(6, code.ExitCode())
	_, human, err := groupedCommentCLI(t, &cliCommentBackend{fail: true}, "moderate", "7", "--hide")
	require.Error(err)
	lines := strings.Split(strings.TrimSpace(human), "\n")
	assert.True(strings.HasPrefix(lines[len(lines)-1], "hint: "))
}

func TestCommentExitClasses(t *testing.T) {
	for _, tc := range []struct {
		code string
		exit int
	}{
		{"message_not_found", 4}, {"action_not_enabled", 5},
		{"private_reply_window_closed", 6}, {"already_sent", 6},
		{"token_invalid", 7}, {"permission_missing", 7}, {"app_access_insufficient", 7},
	} {
		t.Run(tc.code, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			backend := &cliCommentBackend{failure: &meta.ActionError{Code: tc.code, Message: "Example refusal"}}
			out, stderr, err := groupedCommentCLI(t, backend, "moderate", "7", "--hide", "--json")
			require.Error(err)
			assert.Empty(out)
			var diagnostic struct {
				Error struct {
					Code string `json:"code"`
					Exit int    `json:"exit_code"`
				} `json:"error"`
			}
			require.NoError(json.Unmarshal([]byte(stderr), &diagnostic))
			assert.Equal(tc.code, diagnostic.Error.Code)
			assert.Equal(tc.exit, diagnostic.Error.Exit)
		})
	}
}

func TestCommentJSONModeRespectsFlagValues(t *testing.T) {
	assert := assert.New(t)
	for _, args := range [][]string{
		{"queue", "--source", "--json", "--limit", "bad"},
		{"queue", "--json", "--typo", "--json=false"},
		{"reply", "7", "--body", "--json", "--visibility", "invalid"},
	} {
		out, stderr, err := groupedCommentCLI(t, &cliCommentBackend{}, args...)
		require.Error(t, err)
		assert.Empty(out)
		assert.Contains(stderr, "hint: ")
		assert.NotContains(stderr, `{"error":`)
	}
}

func TestCommentDaemonContentionDiagnostic(t *testing.T) {
	for _, args := range [][]string{{"moderate", "7", "--hide"}, {"triage", "7", "--state", "handled"}} {
		for _, jsonMode := range []bool{false, true} {
			name := strings.Join(args, " ")
			if jsonMode {
				name += " json"
			}
			t.Run(name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				calls := 0
				client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/health" {
						assert.NoError(json.MarshalWrite(w, map[string]string{"status": "ok", "api_schema_version": commentActionsMinAPISchemaVersion}))
						return
					}
					calls++
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"Another operation is active"}`))
				})
				commandArgs := append([]string(nil), args...)
				if jsonMode {
					commandArgs = append(commandArgs, "--json")
				}
				out, stderr, err := groupedCommentCLI(t, client, commandArgs...)
				require.Error(err)
				assert.Empty(out)
				var coded interface{ ExitCode() int }
				require.ErrorAs(err, &coded)
				assert.Equal(6, coded.ExitCode())
				assert.Equal(1, calls, "contention must not retry a mutation automatically")
				if jsonMode {
					var diagnostic struct {
						Error struct {
							Code string `json:"code"`
							Exit int    `json:"exit_code"`
							Hint string `json:"hint"`
						} `json:"error"`
					}
					require.NoError(json.Unmarshal([]byte(stderr), &diagnostic))
					assert.Equal("operation_in_progress", diagnostic.Error.Code)
					assert.Equal(6, diagnostic.Error.Exit)
					assert.Contains(diagnostic.Error.Hint, "wait")
					assert.Contains(diagnostic.Error.Hint, "retry")
				} else {
					lines := strings.Split(strings.TrimSpace(stderr), "\n")
					assert.Contains(lines[len(lines)-1], "hint: wait")
				}
			})
		}
	}
}
