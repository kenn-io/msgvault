package cmd

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/daemonclient"
)

const (
	cliStreamStdout = "stdout"
	cliStreamStderr = "stderr"
)

// loggingPassthroughFlags are root persistent flags whose values express
// per-invocation logging intent (verbosity, SQL tracing). Every other root
// persistent flag is stripped from CLIRunRequest.Args because the daemon
// re-resolves configuration from its own environment; these must instead
// survive into the request so an explicit `msgvault --log-level debug <cmd>`
// on the client is honored by the daemon-spawned CLI subprocess.
var loggingPassthroughFlags = map[string]bool{
	"log-level":       true,
	"verbose":         true,
	"log-sql":         true,
	"log-sql-slow-ms": true,
}

func runDaemonCLICommandHTTPFromCobra(cmd *cobra.Command, args []string) error {
	return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, nil)
}

func runDaemonCLICommandHTTPFromCobraWithEnv(cmd *cobra.Command, args []string, env map[string]string) error {
	runArgs, err := daemonCLIArgsFromCobra(cmd, args)
	if err != nil {
		return err
	}
	return runDaemonCLICommandHTTPWithEnv(cmd, runArgs, env)
}

func runDaemonCLICommandHTTPWithEnv(cmd *cobra.Command, args []string, env map[string]string) error {
	st, info, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	cwd, err := daemonCLIRunCwd(info)
	if err != nil {
		return err
	}

	runErr := st.RunCLICommand(cmd.Context(), daemonclient.CLIRunRequest{Args: args, Env: env, Cwd: cwd}, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			if _, err := fmt.Fprint(cmd.OutOrStdout(), data); err != nil {
				return fmt.Errorf("write CLI stdout: %w", err)
			}
		case cliStreamStderr:
			if _, err := fmt.Fprint(cmd.ErrOrStderr(), data); err != nil {
				return fmt.Errorf("write CLI stderr: %w", err)
			}
		}
		return nil
	})
	if runErr != nil && strings.Contains(runErr.Error(), cliSubprocessExitSentinel) {
		// The daemon subprocess already streamed its real error to our
		// stderr and exited non-zero. Propagate a non-zero exit without
		// letting cobra print a second, redundant error line.
		cmd.SilenceErrors = true
		return errCLISubprocessProxied
	}
	return runErr
}

// errCLISubprocessProxied signals that a daemon-proxied CLI subprocess ran and
// failed. The real error was already shown to the user via streamed stderr, so
// callers exit non-zero without printing anything further.
var errCLISubprocessProxied = errors.New("cli subprocess failed")

func daemonCLIRunCwd(info HTTPStoreInfo) (string, error) {
	if info.Kind != HTTPStoreLocalDaemon {
		return "", nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	return cwd, nil
}

func daemonCLIArgsFromCobra(cmd *cobra.Command, args []string) ([]string, error) {
	if cmd == nil {
		return nil, errors.New("missing command")
	}
	runArgs := daemonCLICommandPath(cmd)

	flags := make([]*pflag.Flag, 0)
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if isRootPersistentFlag(cmd, flag) && !loggingPassthroughFlags[flag.Name] {
			return
		}
		flags = append(flags, flag)
	})
	sort.Slice(flags, func(i, j int) bool {
		return flags[i].Name < flags[j].Name
	})
	for _, flag := range flags {
		runArgs = appendDaemonCLIFlag(runArgs, flag)
	}

	runArgs = append(runArgs, args...)
	return runArgs, nil
}

func daemonCLICommandPath(cmd *cobra.Command) []string {
	if !cmd.HasParent() {
		return []string{cmd.Name()}
	}
	var parts []string
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		parts = append(parts, c.Name())
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}

func isRootPersistentFlag(cmd *cobra.Command, flag *pflag.Flag) bool {
	if cmd == nil || flag == nil || cmd.Root() == nil {
		return false
	}
	return cmd.Root().PersistentFlags().Lookup(flag.Name) != nil
}

func appendDaemonCLIFlag(args []string, flag *pflag.Flag) []string {
	name := "--" + flag.Name
	if flag.Value.Type() == "bool" {
		if flag.Value.String() == "true" {
			return append(args, name)
		}
		return append(args, name+"="+flag.Value.String())
	}
	if sliceValue, ok := flag.Value.(pflag.SliceValue); ok {
		values := sliceValue.GetSlice()
		if len(values) == 0 {
			return append(args, name+"=")
		}
		for _, value := range values {
			args = append(args, name+"="+value)
		}
		return args
	}
	return append(args, name+"="+flag.Value.String())
}
