package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIErrorRendering(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		hint string
	}{
		{[]string{"missing"}, 2, "msgvault --help"},
		{[]string{"probe", "--nope"}, 2, "msgvault probe --help"},
		{[]string{"probe"}, 2, "msgvault probe <id>"},
		{[]string{"probe", "1", "--count=nope"}, 3, "msgvault probe --help"},
		{[]string{"probe", "1"}, 2, "msgvault probe --help"},
	} {
		for _, jsonMode := range []bool{false, true} {
			t.Run(fmt.Sprint(tc.args, jsonMode), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				root := newRootCommand()
				probe := &cobra.Command{Use: "probe <id>", Args: cobra.ExactArgs(1), RunE: func(*cobra.Command, []string) error { return nil }}
				probe.Flags().Bool("json", false, "")
				probe.Flags().Int("count", 0, "")
				probe.Flags().String("required", "", "")
				require.NoError(probe.MarkFlagRequired("required"))
				root.AddCommand(probe)
				var out, stderr bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&stderr)
				args := append([]string{}, tc.args...)
				if jsonMode {
					args = append(args, "--json")
				}
				root.SetArgs(args)
				err := executeRootContext(context.Background(), root, args)
				require.Error(err)
				assert.Equal(tc.code, ExitCode(err))
				assert.Empty(out.String())
				assert.NotContains(stderr.String(), "Usage:")
				assert.Contains(stderr.String(), tc.hint)
				if jsonMode && tc.args[0] != "missing" {
					var got struct {
						Error struct {
							Hint     string `json:"hint"`
							ExitCode int    `json:"exit_code"`
						} `json:"error"`
					}
					require.NoError(json.Unmarshal(stderr.Bytes(), &got))
					assert.Equal(tc.code, got.Error.ExitCode)
				}
			})
		}
	}
}

func TestCLIErrorCauseAndExit(t *testing.T) {
	cause := errors.New("fixture")
	for _, tc := range []struct {
		err  error
		code int
	}{
		{cause, 1}, {usageErr(nil, cause), 2}, {validationErr(nil, cause), 3},
		{&cliError{Code: "not_found", ExitCode: 4, Err: cause}, 4},
		{&cliError{Code: "not_initialized", ExitCode: 5, Err: cause}, 5},
		{&cliError{Code: "conflict", ExitCode: 6, Err: cause}, 6},
		{&cliError{Code: "unavailable", ExitCode: 7, Err: cause}, 7}, {ErrSyncPaused, 75},
	} {
		assert.Equal(t, tc.code, ExitCode(fmt.Errorf("outer: %w", withHint(tc.err, "try again"))))
		assert.ErrorIs(t, withHint(tc.err, "try again"), tc.err)
	}
}

func TestCLIErrorPanicAndCancellation(t *testing.T) {
	for _, scenario := range []string{"panic", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			root := newRootCommand()
			root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { prepareInvocation(cmd); return nil }
			command := &cobra.Command{Use: "probe", RunE: func(*cobra.Command, []string) error {
				if scenario == "panic" {
					panic("fixture panic")
				}
				cancel()
				return ctx.Err()
			}}
			command.Flags().Bool("json", false, "")
			command.Flags().BoolP("verbose", "v", false, "")
			command.Flags().IntP("count", "n", 0, "")
			root.AddCommand(command)
			var out, diagnostic bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&diagnostic)
			args := []string{"probe", "--json"}
			root.SetArgs(args)
			err := executeRootContext(ctx, root, args)
			require.Error(err)
			var got struct {
				Error cliErrorRecord `json:"error"`
			}
			require.NoError(json.Unmarshal(diagnostic.Bytes(), &got))
			want := 1
			if scenario == "cancel" {
				want = 130
			}
			assert.Equal(want, ExitCode(err))
			assert.Equal(want, got.Error.ExitCode)
			assert.Empty(out.String())
		})
	}
}

func TestInvalidLogLevelIsValidation(t *testing.T) {
	root := newRootCommand()
	root.AddCommand(&cobra.Command{Use: "search", RunE: func(*cobra.Command, []string) error {
		assert.Fail(t, "invalid log level must stop before running the handler")
		return nil
	}})
	args := []string{"--home", t.TempDir(), "--log-level", "bogus", "search"}
	root.SetArgs(args)
	var diagnostic bytes.Buffer
	root.SetErr(&diagnostic)
	err := executeRootContext(t.Context(), root, args)
	require.Error(t, err)
	assert.Equal(t, 3, ExitCode(err))
	assert.Contains(t, diagnostic.String(), "msgvault search --help")
}
