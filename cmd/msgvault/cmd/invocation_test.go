package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/logging"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/scheduler"
)

func testConfigValue() *config.Config { return config.NewDefaultConfig() }

func testDiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testInvocationWithConfig(cfg *config.Config) *invocation {
	return &invocation{cfg: cfg, logger: testDiscardLogger()}
}

func testLoggerValue() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testInvocationContext(ctx context.Context, cfg *config.Config, options invocationOptions) context.Context {
	state := newInvocation()
	state.cfg = cfg
	state.options = options
	return withInvocation(ctx, state)
}

func TestInvocationBoundJobRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	state := newInvocation()
	state.cfg = testConfigValue()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job := scheduler.Job{
		Run: invocationBoundJobRun(state, func(ctx context.Context) error {
			assert.Same(state, invocationFromContext(ctx))
			assert.Same(state.cfg, invocationFromContext(ctx).cfg)
			assert.ErrorIs(ctx.Err(), context.Canceled)
			return nil
		}),
	}
	require.NoError(job.Run(ctx))
}

func TestInvocationIsolation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	defaultHome := t.TempDir()
	t.Setenv("MSGVAULT_HOME", defaultHome)
	explicitHome := t.TempDir()
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	var homes []string
	root.AddCommand(&cobra.Command{
		Use:  "probe",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			inv := invocationFromCommand(command)
			require.NotNil(inv)
			require.NotNil(inv.cfg)
			homes = append(homes, inv.cfg.HomeDir)
			return nil
		},
	})

	root.SetArgs([]string{"--home", explicitHome, "--no-log-file", "probe"})
	require.NoError(executeRootContext(context.Background(), root))
	root.SetArgs([]string{"probe"})
	require.NoError(executeRootContext(context.Background(), root))

	require.Len(homes, 2)
	assert.Equal(explicitHome, homes[0])
	assert.Equal(defaultHome, homes[1])
}

func TestInvocationRepeatedExecution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	defaultHome := t.TempDir()
	explicitHome := t.TempDir()
	t.Setenv("MSGVAULT_HOME", defaultHome)

	root := rootCmd
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	var seenHomes []string
	var seenInvocations []*invocation
	probe := &cobra.Command{
		Use:  "invocation-probe",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			inv := invocationFromCommand(command)
			require.NotNil(inv)
			require.NotNil(inv.cfg)
			require.NotNil(inv.logResult)
			seenInvocations = append(seenInvocations, inv)
			seenHomes = append(seenHomes, inv.cfg.HomeDir)
			return nil
		},
	}
	root.AddCommand(probe)
	t.Cleanup(func() { root.RemoveCommand(probe) })

	root.SetArgs([]string{"--home", explicitHome, "--no-log-file", "invocation-probe"})
	require.NoError(executeRootContext(context.Background(), root))
	root.SetArgs([]string{"invocation-probe"})
	require.NoError(executeRootContext(context.Background(), root))

	require.Len(seenHomes, 2)
	assert.Equal(explicitHome, seenHomes[0])
	assert.Equal(defaultHome, seenHomes[1])
	assert.NotSame(seenInvocations[0], seenInvocations[1])
}

func TestInvocationLogLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	var skippedConfig, skippedResult bool
	root.AddCommand(&cobra.Command{
		Use:  "version",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			inv := invocationFromCommand(command)
			require.NotNil(inv)
			skippedConfig = inv.cfg != nil
			skippedResult = inv.logResult != nil
			return nil
		},
	})
	root.AddCommand(&cobra.Command{
		Use:  "probe <mode>",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if args[0] == "error" {
				return errors.New("probe failure")
			}
			slog.Info("probe body", "mode", args[0])
			return nil
		},
	})
	home := t.TempDir()
	for _, test := range []struct {
		mode, outcome string
	}{
		{mode: "ok", outcome: "ok"},
		{mode: "error", outcome: "error"},
	} {
		logPath := filepath.Join(t.TempDir(), test.mode+".log")
		root.SetArgs([]string{
			"--home", home, "--log-file", logPath, "probe", test.mode,
		})
		err := executeRootContext(context.Background(), root)
		if test.outcome == "ok" {
			require.NoError(err)
		} else {
			require.ErrorContains(err, "probe failure")
		}
		data, readErr := os.ReadFile(logPath)
		require.NoError(readErr)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		require.NotEmpty(lines)
		last := lines[len(lines)-1]
		assert.Contains(last, `"msg":"msgvault exit"`)
		assert.Contains(last, `"outcome":"`+test.outcome+`"`)
		require.NoError(os.Rename(logPath, logPath+".closed"), "log file must be closed after execution")
	}

	skippedHome := t.TempDir()
	root.SetArgs([]string{"--home", skippedHome, "version"})
	require.NoError(executeRootContext(context.Background(), root))
	assert.False(skippedConfig)
	assert.False(skippedResult)
	assert.NoDirExists(filepath.Join(skippedHome, "logs"))
	assert.NoFileExists(filepath.Join(skippedHome, "config.toml"))

	badConfig := filepath.Join(t.TempDir(), "bad.toml")
	require.NoError(os.WriteFile(badConfig, []byte("not = [valid"), 0o600))
	root.SetArgs([]string{"--config", badConfig, "probe", "ok"})
	require.Error(executeRootContext(context.Background(), root))
	inv := invocationFromContext(root.Context())
	require.NotNil(inv)
	assert.Nil(inv.logResult, "a failed load must not reuse a prior logging result")
	assert.Nil(inv.cfg, "a failed load must not reuse a prior config")
}

func TestInvocationPanicCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	logPath := filepath.Join(t.TempDir(), "panic.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestInvocationPanicHelper$") //nolint:gosec // test binary and fixed test selector.
	cmd.Env = append(os.Environ(),
		"MSGVAULT_INVOCATION_PANIC_HELPER=1",
		"MSGVAULT_INVOCATION_PANIC_LOG="+logPath,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(err, &exitErr)
	assert.Equal(2, exitErr.ExitCode(), string(output))
	data, readErr := os.ReadFile(logPath)
	require.NoError(readErr)
	assert.Contains(string(data), `"msg":"msgvault panic"`)
	require.NoError(os.Rename(logPath, logPath+".closed"), "panic cleanup must close the log file")
}

func TestInvocationPanicHelper(t *testing.T) {
	if os.Getenv("MSGVAULT_INVOCATION_PANIC_HELPER") != "1" {
		return
	}
	root := newRootCommand()
	root.AddCommand(&cobra.Command{
		Use: "panic",
		RunE: func(*cobra.Command, []string) error {
			panic("invocation panic")
		},
	})
	root.SetArgs([]string{
		"--home", filepath.Dir(os.Getenv("MSGVAULT_INVOCATION_PANIC_LOG")),
		"--log-file", os.Getenv("MSGVAULT_INVOCATION_PANIC_LOG"), "panic",
	})
	_ = executeRootContext(context.Background(), root)
	os.Exit(3)
}

func TestTUILoggerRestoration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	logPath := filepath.Join(t.TempDir(), "tui.log")
	result, err := logging.BuildHandler(logging.Options{
		FilePath: logPath, Stderr: io.Discard,
	})
	require.NoError(err)
	t.Cleanup(result.Close)
	previous := slog.New(slog.DiscardHandler)
	slog.SetDefault(previous)
	t.Cleanup(func() { slog.SetDefault(previous) })
	inside := false
	require.NoError(withTUIFileLogger(result, func() error {
		inside = slog.Default() != previous
		return nil
	}))
	assert.True(inside)
	assert.Same(previous, slog.Default())
}

func TestInvocationUsageContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	newRoot := func(command *cobra.Command, args ...string) (string, error) {
		root := newRootCommand()
		var stderr bytes.Buffer
		root.SetOut(&stderr)
		root.SetErr(&stderr)
		root.AddCommand(command)
		root.SetArgs(args)
		err := executeRootContext(context.Background(), root)
		return stderr.String(), err
	}

	usage, err := newRoot(&cobra.Command{
		Use:  "required <value>",
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error { return nil },
	}, "required")
	require.Error(err)
	assert.Contains(usage, "Usage:")

	usage, err = newRoot(&cobra.Command{
		Use: "contract",
		RunE: func(command *cobra.Command, _ []string) error {
			return usageErr(command, errors.New("contract failure"))
		},
	}, "--no-log-file", "--log-level", "error", "contract")
	require.ErrorContains(err, "contract failure")
	assert.Contains(usage, "Usage:")

	usage, err = newRoot(&cobra.Command{
		Use:  "runtime",
		RunE: func(*cobra.Command, []string) error { return errors.New("runtime failure") },
	}, "--no-log-file", "--log-level", "error", "runtime")
	require.ErrorContains(err, "runtime failure")
	assert.NotContains(usage, "Usage:")
}

func TestInvocationMCPStatusOptions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	defaultHome := t.TempDir()
	homeA := t.TempDir()
	homeB := t.TempDir()
	t.Setenv("MSGVAULT_HOME", defaultHome)

	publish := func(home, address string) func() error {
		cleanup, err := mcpdiscovery.Publish(filepath.Join(home, "mcp"), address, "", "")
		require.NoError(err)
		return cleanup
	}
	cleanupDefault := publish(defaultHome, "127.0.0.1:9100")
	defer func() { require.NoError(cleanupDefault()) }()
	cleanupA := publish(homeA, "127.0.0.1:9101")
	defer func() { require.NoError(cleanupA()) }()
	cleanupB := publish(homeB, "127.0.0.1:9102")
	defer func() { require.NoError(cleanupB()) }()

	configA := filepath.Join(t.TempDir(), "a.toml")
	configB := filepath.Join(t.TempDir(), "b.toml")
	require.NoError(os.WriteFile(configA, []byte("[server]\napi_port = 9103\n"), 0o600))
	require.NoError(os.WriteFile(configB, []byte("[server]\napi_port = 9104\n"), 0o600))
	cleanupConfigA := publish(filepath.Dir(configA), "127.0.0.1:9103")
	defer func() { require.NoError(cleanupConfigA()) }()
	cleanupConfigB := publish(filepath.Dir(configB), "127.0.0.1:9104")
	defer func() { require.NoError(cleanupConfigB()) }()

	root := rootCmd
	root.SetErr(io.Discard)
	root.SetOut(io.Discard)
	status := func(args ...string) mcpdiscovery.Endpoint {
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetArgs(args)
		require.NoError(executeRootContext(context.Background(), root))
		var endpoints []mcpdiscovery.Endpoint
		require.NoError(json.Unmarshal(output.Bytes(), &endpoints))
		require.Len(endpoints, 1)
		return endpoints[0]
	}

	assert.Equal("http://127.0.0.1:9101/mcp", status("--home", homeA, "mcp", "status", "--json").URL)
	assert.Equal("http://127.0.0.1:9102/mcp", status("--home", homeB, "mcp", "status", "--json").URL)
	assert.Equal("http://127.0.0.1:9103/mcp", status("--config", configA, "mcp", "status", "--json").URL)
	assert.Equal("http://127.0.0.1:9104/mcp", status("--config", configB, "mcp", "status", "--json").URL)
	assert.Equal("http://127.0.0.1:9100/mcp", status("mcp", "status", "--json").URL)

	for _, home := range []string{homeA, homeB, filepath.Dir(configA), filepath.Dir(configB), defaultHome} {
		assert.NoDirExists(filepath.Join(home, "logs"))
	}
	for _, home := range []string{homeA, homeB, defaultHome} {
		assert.NoFileExists(filepath.Join(home, "config.toml"))
	}
}
