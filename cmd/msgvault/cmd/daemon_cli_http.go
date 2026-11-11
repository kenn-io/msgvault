package cmd

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/cligrammar"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/daemonclient"
)

const (
	cliStreamStdout = "stdout"
	cliStreamStderr = "stderr"
)

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

func runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd *cobra.Command, args []string) error {
	runArgs, err := daemonCLIArgsFromCobra(cmd, args)
	if err != nil {
		return err
	}
	return runDaemonCLICommandHTTPWithEnv(cmd, runArgs, nil, true, false)
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

	var paused bool
	isDraft := api.IsCLIRunDraftCreate(args) || api.IsCLIRunDraftLifecycle(args) || api.IsCLIRunDraftSendAs(args)
	// In-process drafts can return reconciliation data on failure. Buffer that
	// diagnostic so it becomes details of the one terminal error, not a second
	// JSON object. Success stdout and successful stderr retain their payloads.
	var draftStderr bytes.Buffer
	stderr := &childStderr{w: &draftStderr}
	if !isDraft {
		stderr, err = newChildStderr(cmd)
		if err != nil {
			return err
		}
		defer stderr.close()
	}
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
		}
		return nil
	})
	if isDraft {
		if runErr != nil {
			diagnostic := classifyCLIError(cmd, runErr)
			if data := bytes.TrimSpace(draftStderr.Bytes()); len(data) > 0 {
				raw := jsontext.Value(bytes.Clone(data))
				if raw.IsValid() {
					diagnostic.Details = raw
				} else {
					diagnostic.Details, _ = json.Marshal(string(data), jsontext.AllowInvalidUTF8(true))
				}
			}
			return diagnostic
		}
		if _, err := draftStderr.WriteTo(cmd.ErrOrStderr()); err != nil {
			return fmt.Errorf("write draft diagnostics: %w", err)
		}
	}
	runErr = stderr.finish(cmd, runErr)
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
	w        io.Writer
	tail     []byte
	deferred *os.File
}

func newChildStderr(cmd *cobra.Command) (*childStderr, error) {
	stderr := &childStderr{w: cmd.ErrOrStderr()}
	state := invocationFromCommand(cmd)
	if state != nil && state.cfg != nil && state.cfg.DaemonConfigError() != nil {
		// Spool only while an unrelated configuration warning is pending. A
		// private temporary file keeps arbitrarily long stderr out of memory.
		file, err := os.CreateTemp("", "msgvault-cli-stderr-*")
		if err != nil {
			return nil, fmt.Errorf("defer CLI stderr: %w", err)
		}
		stderr.deferred = file
		stderr.w = file
	}
	return stderr, nil
}

func (s *childStderr) close() {
	if s.deferred != nil {
		_ = s.deferred.Close()
		_ = os.Remove(s.deferred.Name())
	}
}

func (s *childStderr) finish(cmd *cobra.Command, runErr error) error {
	proxy, proxied := proxiedChildFailure(s, runErr)
	if s.deferred != nil {
		if proxied && proxy.alreadyReported() {
			state := invocationFromCommand(cmd)
			warnDaemonConfigError(cmd.Root(), state.cfg, proxy)
			proxy.configWarningsReported = true
		}
		if _, err := s.deferred.Seek(0, io.SeekStart); err != nil {
			return errors.Join(runErr, fmt.Errorf("read deferred CLI stderr: %w", err))
		}
		if _, err := io.Copy(cmd.ErrOrStderr(), s.deferred); err != nil {
			return errors.Join(runErr, fmt.Errorf("write CLI stderr: %w", err))
		}
	}
	if proxied {
		return proxy
	}
	return runErr
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
type proxiedChildError struct {
	stderr                 *childStderr
	code                   int
	errorReported          bool
	configWarningsReported bool
}

func (e proxiedChildError) ExitCode() int {
	if e.code > 0 {
		return e.code
	}
	return 1
}

func proxiedChildFailure(stderr *childStderr, err error) (proxiedChildError, bool) {
	if err == nil || !strings.Contains(err.Error(), cliSubprocessExitSentinel) {
		return proxiedChildError{}, false
	}
	reported := false
	if child, ok := errors.AsType[interface {
		error
		ErrorReported() bool
	}](err); ok {
		reported = child.ErrorReported()
	}
	return proxiedChildError{stderr: stderr, code: ExitCode(err), errorReported: reported}, true
}

func (e proxiedChildError) alreadyReported() bool {
	if e.errorReported {
		return true
	}
	if e.stderr == nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(e.stderr.tail)), "\n")
	if len(lines) == 0 {
		return false
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	// Old daemons cannot send metadata. Only a complete terminal diagnostic
	// qualifies; an informational line merely quoting JSON never does.
	var record struct {
		Error struct {
			Code     string `json:"code"`
			Message  string `json:"message"`
			Hint     string `json:"hint"`
			ExitCode int    `json:"exit_code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(last), &record) == nil && record.Error.Code != "" && record.Error.Hint != "" && record.Error.ExitCode > 0 {
		return true
	}
	if strings.HasPrefix(last, "hint:") {
		for _, line := range slices.Backward(lines[:len(lines)-1]) {
			if strings.HasPrefix(line, "Error:") {
				return true
			}
		}
	}
	return false
}

// legacyMessage finds the final human diagnostic from a pre-hint daemon.
// New daemons use explicit metadata instead of inspecting their stderr text.
func (e proxiedChildError) legacyMessage() string {
	if e.stderr == nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(e.stderr.tail)), "\n")
	for _, line := range slices.Backward(lines) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "time=") && strings.Contains(line, `msg="msgvault exit"`) {
			continue
		}
		if message, ok := strings.CutPrefix(line, "Error: "); ok {
			return message
		}
		return ""
	}
	return ""
}

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
	runArgs := cligrammar.Legacy(daemonCLICommandPath(cmd))

	flags := make([]*pflag.Flag, 0)
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if !flag.Changed {
			return
		}
		if flag.Name == addAccountGrantDecidedFlag {
			return
		}
		// Every root persistent flag except the logging flags is stripped,
		// because the daemon re-resolves configuration from its own
		// environment. The logging flags express per-invocation intent, so
		// an explicit `msgvault --log-level debug <cmd>` reaches the
		// daemon-spawned CLI subprocess.
		if isRootPersistentFlag(cmd, flag) && !clirun.IsLoggingFlag(flag.Name) {
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

	if n := cmd.ArgsLenAtDash(); n >= 0 {
		runArgs = append(append(runArgs, args[:n]...), "--")
		args = args[n:]
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
