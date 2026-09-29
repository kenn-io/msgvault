package cmd

import (
	"context"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/logging"
)

// invocationOptions is the set of root-owned options that belong to one
// execution. Cobra owns the parsed flag values; this snapshot survives child
// command forwarding and delayed work after the command tree has moved on.
type invocationOptions struct {
	cfgFile    string
	homeDir    string
	verbose    bool
	useLocal   bool
	logFile    string
	logLevel   string
	noLogFile  bool
	logSQL     bool
	logSQLSlow int64

	cfgFileChanged  bool
	homeDirChanged  bool
	verboseChanged  bool
	useLocalChanged bool
	logFileChanged  bool
	logLevelChanged bool
	noLogChanged    bool
	logSQLChanged   bool
	logSlowChanged  bool

	agentURL           string
	agentTokenFile     string
	agentAllowInsecure bool
	agentURLChanged    bool
	agentTokenChanged  bool
}

type invocation struct {
	options   invocationOptions
	cfg       *config.Config
	logger    *slog.Logger
	logResult *logging.Result
}

type invocationContextKey struct{}

func newInvocation() *invocation {
	return &invocation{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

func withInvocation(ctx context.Context, inv *invocation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, invocationContextKey{}, inv)
}

func invocationBoundJobRun(inv *invocation, run func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		return run(withInvocation(ctx, inv))
	}
}

func invocationFromContext(ctx context.Context) *invocation {
	if ctx == nil {
		return nil
	}
	inv, _ := ctx.Value(invocationContextKey{}).(*invocation)
	return inv
}

func invocationFromCommand(cmd *cobra.Command) *invocation {
	if cmd == nil {
		return nil
	}
	if inv := invocationFromContext(cmd.Context()); inv != nil {
		return inv
	}
	return invocationFromContext(cmd.Root().Context())
}

func loggerFromContext(ctx context.Context) *slog.Logger {
	if inv := invocationFromContext(ctx); inv != nil && inv.logger != nil {
		return inv.logger
	}
	return slog.New(slog.DiscardHandler)
}

func optionsFromContext(ctx context.Context) invocationOptions {
	if inv := invocationFromContext(ctx); inv != nil {
		return inv.options
	}
	return invocationOptions{}
}

func prepareInvocation(cmd *cobra.Command) *invocation {
	if cmd == nil {
		return nil
	}
	root := cmd.Root()
	inv := invocationFromContext(root.Context())
	if inv == nil {
		inv = newInvocation()
		root.SetContext(withInvocation(root.Context(), inv))
	}
	if inv.logger == nil {
		inv.logger = newInvocation().logger
	}

	// Read root-owned options from the executing root. A reusable leaf can
	// retain inherited flags from an earlier parent, so cmd.Flags() may expose
	// stale values from another invocation tree.
	flags := root.PersistentFlags()
	if cmd == root && flags.Lookup("agent-url") == nil {
		flags = cmd.Flags()
	}
	if flags.Lookup("config") != nil {
		inv.options.cfgFile = invocationStringFlag(flags, "config")
	}
	if flags.Lookup("home") != nil {
		inv.options.homeDir = invocationStringFlag(flags, "home")
	}
	if flags.Lookup(flagVerbose) != nil {
		inv.options.verbose = invocationBoolFlag(flags, flagVerbose)
	}
	if flags.Lookup(localValue) != nil {
		inv.options.useLocal = invocationBoolFlag(flags, localValue)
	}
	if flags.Lookup("log-file") != nil {
		inv.options.logFile = invocationStringFlag(flags, "log-file")
	}
	if flags.Lookup("log-level") != nil {
		inv.options.logLevel = invocationStringFlag(flags, "log-level")
	}
	if flags.Lookup("no-log-file") != nil {
		inv.options.noLogFile = invocationBoolFlag(flags, "no-log-file")
	}
	if flags.Lookup(flagLogSQL) != nil {
		inv.options.logSQL = invocationBoolFlag(flags, flagLogSQL)
	}
	if flags.Lookup("log-sql-slow-ms") != nil {
		inv.options.logSQLSlow = invocationInt64Flag(flags, "log-sql-slow-ms")
	}
	if flags.Lookup("agent-url") != nil {
		inv.options.agentURL = invocationStringFlag(flags, "agent-url")
	}
	if flags.Lookup("agent-token-file") != nil {
		inv.options.agentTokenFile = invocationStringFlag(flags, "agent-token-file")
	}
	if flags.Lookup("agent-allow-insecure") != nil {
		inv.options.agentAllowInsecure = invocationBoolFlag(flags, "agent-allow-insecure")
	}
	if flag := flags.Lookup("agent-url"); flag != nil {
		inv.options.agentURLChanged = flag.Changed
	}
	if flag := flags.Lookup("agent-token-file"); flag != nil {
		inv.options.agentTokenChanged = flag.Changed
	}
	inv.options.cfgFileChanged = invocationFlagChanged(flags, "config")
	inv.options.homeDirChanged = invocationFlagChanged(flags, "home")
	inv.options.verboseChanged = invocationFlagChanged(flags, flagVerbose)
	inv.options.useLocalChanged = invocationFlagChanged(flags, localValue)
	inv.options.logFileChanged = invocationFlagChanged(flags, "log-file")
	inv.options.logLevelChanged = invocationFlagChanged(flags, "log-level")
	inv.options.noLogChanged = invocationFlagChanged(flags, "no-log-file")
	inv.options.logSQLChanged = invocationFlagChanged(flags, flagLogSQL)
	inv.options.logSlowChanged = invocationFlagChanged(flags, "log-sql-slow-ms")

	// Cobra retains a leaf's context between Execute calls. Refresh it from
	// the root so a repeated execution cannot keep the prior run's state.
	cmd.SetContext(root.Context())
	return inv
}

func invocationStringFlag(flags *pflag.FlagSet, name string) string {
	if flags == nil {
		return ""
	}
	value, err := flags.GetString(name)
	if err != nil {
		return ""
	}
	return value
}

func invocationBoolFlag(flags *pflag.FlagSet, name string) bool {
	if flags == nil {
		return false
	}
	value, err := flags.GetBool(name)
	if err != nil {
		return false
	}
	return value
}

func invocationInt64Flag(flags *pflag.FlagSet, name string) int64 {
	if flags == nil {
		return 0
	}
	value, err := flags.GetInt64(name)
	if err != nil {
		return 0
	}
	return value
}

func invocationFlagChanged(flags *pflag.FlagSet, name string) bool {
	if flags == nil {
		return false
	}
	flag := flags.Lookup(name)
	return flag != nil && flag.Changed
}

func clearInvocationFlags(root *cobra.Command) {
	if root == nil {
		return
	}
	for _, name := range []string{
		"config", "home", flagVerbose, localValue, "log-file", "log-level",
		"no-log-file", flagLogSQL, "log-sql-slow-ms", "agent-url",
		"agent-token-file", "agent-allow-insecure",
	} {
		if flag := root.PersistentFlags().Lookup(name); flag != nil {
			_ = flag.Value.Set(flag.DefValue)
			flag.Changed = false
		}
	}
}
