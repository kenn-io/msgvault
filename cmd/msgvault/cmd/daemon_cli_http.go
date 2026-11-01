package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/api"
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
	return runDaemonCLICommandHTTPWithEnv(cmd, runArgs, env, false, false)
}

func runDaemonCLICommandHTTPFromCobraWithGrantDecision(
	cmd *cobra.Command,
	args []string,
	grantDecided bool,
) error {
	runArgs, err := daemonCLIArgsFromCobra(cmd, args)
	if err != nil {
		return err
	}
	return runDaemonCLICommandHTTPWithEnv(cmd, runArgs, nil, false, grantDecided)
}

func runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd *cobra.Command, args []string, env map[string]string) error {
	runArgs, err := daemonCLIArgsFromCobra(cmd, args)
	if err != nil {
		return err
	}
	return runDaemonCLICommandHTTPWithEnv(cmd, runArgs, env, true, false)
}

func runDaemonCLICommandHTTPWithEnv(
	cmd *cobra.Command,
	args []string,
	env map[string]string,
	requiresLocalFiles bool,
	grantDecided bool,
) error {
	st, info, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	cwd, err := daemonCLIRunCwd(info, requiresLocalFiles)
	if err != nil {
		return err
	}
	if api.IsCLIRunDraftCreate(args) || api.IsCLIRunDraftLifecycle(args) || api.IsCLIRunDraftSendAs(args) {
		cwd = ""
	}

	var draftFailureReported, paused bool
	stderr := &childStderr{w: cmd.ErrOrStderr()}
	runErr := st.RunCLICommand(cmd.Context(), daemonclient.CLIRunRequest{
		Args: args, Env: env, Cwd: cwd, GrantDecided: grantDecided,
	}, func(stream, data string) error {
		switch stream {
		case "outcome":
			paused = paused || data == "paused"
		case cliStreamStdout:
			if _, err := fmt.Fprint(cmd.OutOrStdout(), data); err != nil {
				return fmt.Errorf("write CLI stdout: %w", err)
			}
		case cliStreamStderr:
			if _, err := fmt.Fprint(stderr, data); err != nil {
				return fmt.Errorf("write CLI stderr: %w", err)
			}
			draftFailureReported = (api.IsCLIRunDraftCreate(args) || api.IsCLIRunDraftLifecycle(args) || api.IsCLIRunDraftSendAs(args)) && data != ""
		}
		return nil
	})
	if runErr != nil && draftFailureReported {
		// Draft stderr already contains the failure result or fixed code.
		cmd.SilenceErrors = true
	}
	if runErr != nil && strings.Contains(runErr.Error(), cliSubprocessExitSentinel) {
		// The daemon subprocess already streamed its real error to our
		// stderr and exited non-zero. Propagate a non-zero exit without
		// letting cobra print a second, redundant error line.
		cmd.SilenceErrors = true
		return proxiedChildError{stderr: stderr}
	}
	if runErr == nil && !paused && manualSyncCLICommand(args) {
		reportCLISyncFinished(cmd, st)
	}
	return runErr
}

// errCLISubprocessProxied signals that a daemon-proxied CLI subprocess ran and
// failed. The real error was already shown to the user via streamed stderr, so
// callers exit non-zero without printing anything further.
var errCLISubprocessProxied = errors.New("cli subprocess failed")

// childStderrTail bounds the proxied child stderr kept in memory. The child
// prints its final error last, so the tail holds it.
const childStderrTail = 64 << 10

// childStderr forwards a proxied child's stderr and keeps its tail.
type childStderr struct {
	w    io.Writer
	tail []byte
}

func (s *childStderr) Write(p []byte) (int, error) {
	s.tail = append(s.tail, p...)
	if over := len(s.tail) - childStderrTail; over > 0 {
		s.tail = s.tail[over:]
	}
	return s.w.Write(p)
}

// proxiedChildError is errCLISubprocessProxied plus the stderr the child
// streamed, so the parent can tell which errors the child already showed.
type proxiedChildError struct{ stderr *childStderr }

func (e proxiedChildError) Error() string { return errCLISubprocessProxied.Error() }

func (e proxiedChildError) Unwrap() error { return errCLISubprocessProxied }

// reported reports whether the child's streamed stderr named err's setting.
// It matches only the text before the first ": ", which names the setting,
// because an older daemon's child may word the cause differently.
func (e proxiedChildError) reported(err error) bool {
	setting, _, _ := strings.Cut(err.Error(), ": ")
	return bytes.Contains(e.stderr.tail, []byte(setting))
}

func daemonCLIRunCwd(info HTTPStoreInfo, requiresLocalFiles bool) (string, error) {
	if info.Kind != HTTPStoreLocalDaemon {
		if requiresLocalFiles {
			return "", errors.New("this command uses local files and cannot run through a configured remote daemon; run it on the daemon host with --local")
		}
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
		if !flag.Changed {
			return
		}
		if flag.Name == addAccountGrantDecidedFlag {
			return
		}
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
