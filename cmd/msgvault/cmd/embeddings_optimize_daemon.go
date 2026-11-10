package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/logging"
)

// runCLIEmbeddingsOptimize keeps orchestration in the daemon so preparation
// can yield the gate without a subprocess ownership protocol. Native training
// still runs in its disposable worker process.
func (a *storeAPIAdapter) runCLIEmbeddingsOptimize(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	if len(req.Env) > 0 {
		return errors.New("embeddings optimize does not accept environment overrides")
	}
	return runManagedEmbeddingsCLI(ctx, req, emit, func(root *cobra.Command) {
		group := &cobra.Command{Use: "embeddings"}
		command := &cobra.Command{
			Use: "optimize [generation-id]", Args: cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				closeLogs, err := setupManagedEmbeddingsLogging(cmd)
				if err != nil {
					return err
				}
				defer closeLogs()
				// Acquire this before the gate: another optimize must never wait
				// for the first while holding the lease the first needs to resume.
				if !a.embeddingsOptimizeMu.TryLock() {
					return errors.New("embeddings optimize is already running")
				}
				defer a.embeddingsOptimizeMu.Unlock()
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Waiting for archive operation gate..."); err != nil {
					return fmt.Errorf("report operation gate wait: %w", err)
				}
				lease, err := acquireOptimizeGate(ctx, a.operationGate)
				if err != nil {
					return err
				}
				defer lease.close()
				return runEmbeddingsOptimizeManaged(cmd, args, lease.yield)
			},
		}
		command.Flags().Bool("drop", false, "Remove the accelerator while keeping exact vectors")
		group.AddCommand(command)
		root.AddCommand(group)
	})
}

func setupManagedEmbeddingsLogging(cmd *cobra.Command) (func(), error) {
	state := prepareInvocation(cmd)
	level := state.options.logLevel
	if level == "" && state.cfg != nil {
		level = state.cfg.Log.Level
	}
	if err := logging.ValidateLevel(level); err != nil {
		return nil, err
	}
	var override *slog.Level
	if state.options.verbose {
		debug := slog.LevelDebug
		override = &debug
	}
	logs, err := logging.BuildHandler(logging.Options{
		FileDisabled: true, LevelString: level, LevelOverride: override,
		Stderr: cmd.ErrOrStderr(), HumanConsole: true,
	})
	if err != nil {
		return nil, err
	}
	state.logger = slog.New(logs.Handler)
	return logs.Close, nil
}

func runManagedEmbeddingsCLI(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error, configure func(*cobra.Command)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var outputMu sync.Mutex
	var outputErr error
	writer := func(stream string) optimizeStreamWriter {
		return func(p []byte) (int, error) {
			outputMu.Lock()
			defer outputMu.Unlock()
			if outputErr == nil && emit != nil {
				outputErr = emit(api.CLIRunEvent{Type: stream, Data: string(p)})
			}
			if outputErr != nil {
				cancel()
				return 0, outputErr
			}
			return len(p), nil
		}
	}
	// This tree is private to the request: never reuse the global Cobra tree
	// or its root initialization, which changes process-wide logging state.
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	clirun.AddLoggingFlags(root.PersistentFlags())
	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		trace, err := root.PersistentFlags().GetBool("log-sql")
		if err != nil {
			return fmt.Errorf("read --log-sql: %w", err)
		}
		slow, err := root.PersistentFlags().GetInt64("log-sql-slow-ms")
		if err != nil {
			return fmt.Errorf("read --log-sql-slow-ms: %w", err)
		}
		if trace || slow > 0 {
			return errors.New("configure daemon SQL logging with [log] sql_trace or sql_slow_ms instead of CLI flags")
		}
		return nil
	}
	configure(root)
	root.SetIn(strings.NewReader(""))
	root.SetOut(writer(cliStreamStdout))
	root.SetErr(writer(cliStreamStderr))
	root.SetArgs(req.Args)
	err := root.ExecuteContext(ctx)
	return errors.Join(err, outputErr, ctx.Err())
}

type optimizeStreamWriter func([]byte) (int, error)

func (w optimizeStreamWriter) Write(p []byte) (int, error) { return w(p) }

type optimizeGateLease struct {
	ctx     context.Context
	gate    api.OperationGate
	release func()
	label   string
}

func acquireOptimizeGate(ctx context.Context, gate api.OperationGate) (*optimizeGateLease, error) {
	return acquireLabeledEmbeddingsGate(ctx, gate, "msgvault embeddings optimize")
}

func acquireLabeledEmbeddingsGate(ctx context.Context, gate api.OperationGate, label string) (*optimizeGateLease, error) {
	lease := &optimizeGateLease{ctx: ctx, gate: gate, label: label}
	if err := lease.acquireRequest(); err != nil {
		return nil, err
	}
	return lease, nil
}

func (l *optimizeGateLease) acquire() error {
	return l.acquireWork(false)
}

func (l *optimizeGateLease) acquireRequest() error {
	return l.acquireWork(true)
}

func (l *optimizeGateLease) acquireWork(request bool) error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	if l.gate == nil {
		return nil
	}
	var ok bool
	if labeled, labeledOK := l.gate.(api.LabeledOperationGate); labeledOK {
		if request {
			l.release, ok = labeled.BeginRequestWorkContext(l.ctx, l.label)
		} else {
			l.release, ok = labeled.BeginLabeledWorkContext(l.ctx, l.label)
		}
	} else {
		l.release, ok = l.gate.BeginWorkContext(l.ctx)
	}
	if ok {
		return nil
	}
	if err := l.ctx.Err(); err != nil {
		return err
	}
	return errors.New("archive is shutting down")
}

func (l *optimizeGateLease) close() {
	if l.release != nil {
		l.release()
		l.release = nil
	}
}

func (l *optimizeGateLease) yield() error {
	l.close()
	// Background admission preserves queued request priority while allowing
	// scheduled archive work to enter at a committed batch boundary too.
	return l.acquire()
}
