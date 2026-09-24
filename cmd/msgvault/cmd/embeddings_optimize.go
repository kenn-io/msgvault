//go:build sqlite_vec

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

var embeddingsOptimizeCmd = &cobra.Command{
	Use:   "optimize [generation-id]",
	Short: "Build or remove the SQLite search accelerator",
	Long: "Build a restartable SQLite approximate-search index from embeddings already stored locally. " +
		"No text or embedding-provider request is made. When omitted, generation-id defaults to the active generation.",
	Args: cobra.MaximumNArgs(1),
	RunE: runEmbeddingsOptimizeCommand,
}

type acceleratorWorkerRunner func(context.Context, string, string, vector.GenerationID, int, io.Writer) error

var runAcceleratorWorkerSubprocess acceleratorWorkerRunner = func(
	ctx context.Context, executable, databasePath string, generationID vector.GenerationID, threads int, stderr io.Writer,
) error {
	// #nosec G204 -- executable is the path of the currently running msgvault binary, not user input.
	worker := exec.CommandContext(ctx, executable, embeddingsCommandName, embeddingsOptimizeWorkerName,
		databasePath, strconv.FormatInt(int64(generationID), 10), strconv.Itoa(threads))
	worker.Stderr = stderr
	if err := worker.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("accelerator worker: %w", err)
	}
	return nil
}

func runEmbeddingsOptimizeCommand(cmd *cobra.Command, args []string) error {
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}
	return runEmbeddingsOptimize(cmd, args)
}

func runEmbeddingsOptimize(cmd *cobra.Command, args []string) error {
	if !cfg.Vector.Enabled {
		return errors.New("vector search not enabled; add [vector] enabled=true to config.toml first")
	}
	if store.IsPostgresURL(cfg.DatabaseDSN()) {
		return errors.New("embeddings optimize is only needed for SQLite; PostgreSQL uses its native vector index")
	}
	release, err := acquireDirectSQLiteWriteLock(cfg)
	if err != nil {
		return err
	}
	defer release()
	if err := ensureMainSchema(); err != nil {
		return err
	}
	backend, closeBackend, err := openEmbeddingsBackend(cmd.Context())
	if err != nil {
		return err
	}
	sqliteBackend, ok := backend.(*sqlitevec.Backend)
	if !ok {
		closeBackend()
		return errors.New("configured vector backend is not SQLite")
	}
	generationID, err := optimizeGenerationID(cmd.Context(), sqliteBackend, args)
	if err != nil {
		closeBackend()
		return err
	}
	drop, err := cmd.Flags().GetBool("drop")
	if err != nil {
		closeBackend()
		return fmt.Errorf("read --drop: %w", err)
	}
	if drop {
		defer closeBackend()
		if err := sqliteBackend.DropAccelerator(cmd.Context(), generationID); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Generation %d accelerator dropped; exact vectors retained.\n", generationID)
		return nil
	}
	plan, err := sqliteBackend.PrepareAccelerator(cmd.Context(), generationID, sqlitevec.OptimizeOptions{
		Threads: cfg.Vector.Search.ANNThreads,
		Progress: func(progress sqlitevec.OptimizeProgress) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Preparing accelerator: %d/%d vectors\n", progress.IndexedCount, progress.TotalCount)
		},
	})
	if err != nil {
		closeBackend()
		return fmt.Errorf("prepare accelerator: %w", err)
	}
	if !plan.Applicable {
		closeBackend()
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Generation %d remains on exact search: %s.\n", generationID, plan.Reason)
		return nil
	}
	if plan.AlreadyReady {
		closeBackend()
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Generation %d accelerator is already ready (%d vectors).\n", generationID, plan.IndexedCount)
		return nil
	}
	closeBackend()

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve msgvault executable: %w", err)
	}
	vectorPath := cfg.Vector.DBPath
	if vectorPath == "" {
		vectorPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Training accelerator for generation %d with %d thread(s)...\n", generationID, plan.Threads)
	if err := runAcceleratorWorkerSubprocess(cmd.Context(), executable, vectorPath, generationID, plan.Threads, cmd.ErrOrStderr()); err != nil {
		if reopened, closeReopened, reopenErr := openEmbeddingsBackend(context.WithoutCancel(cmd.Context())); reopenErr == nil {
			if concrete, isSQLite := reopened.(*sqlitevec.Backend); isSQLite {
				_ = concrete.RecordAcceleratorError(context.WithoutCancel(cmd.Context()), generationID, err)
			}
			closeReopened()
		}
		return err
	}
	reopened, closeReopened, err := openEmbeddingsBackend(cmd.Context())
	if err != nil {
		return err
	}
	defer closeReopened()
	concrete, ok := reopened.(*sqlitevec.Backend)
	if !ok {
		return errors.New("configured vector backend changed while optimizing")
	}
	status, err := concrete.PublishAccelerator(cmd.Context(), generationID)
	if err != nil {
		_ = concrete.RecordAcceleratorError(context.WithoutCancel(cmd.Context()), generationID, err)
		return fmt.Errorf("publish accelerator: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Generation %d accelerator ready (%d vectors).\n", generationID, status.IndexedCount)
	return nil
}

func optimizeGenerationID(ctx context.Context, backend *sqlitevec.Backend, args []string) (vector.GenerationID, error) {
	if len(args) == 1 {
		return parseGenerationID(args[0])
	}
	active, err := backend.ActiveGeneration(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve active generation: %w", err)
	}
	return active.ID, nil
}
