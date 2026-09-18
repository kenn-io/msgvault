//go:build sqlite_vec

package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

var embeddingsOptimizeWorkerCmd = &cobra.Command{
	Use:    embeddingsOptimizeWorkerName + " <vectors.db> <generation-id> <threads>",
	Args:   cobra.ExactArgs(3),
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		generationID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || generationID <= 0 {
			return fmt.Errorf("invalid accelerator generation %q", args[1])
		}
		threads, err := strconv.Atoi(args[2])
		if err != nil || threads <= 0 {
			return fmt.Errorf("invalid accelerator worker count %q", args[2])
		}
		// The parent owns cancellation by killing this disposable process.
		// Passing Cobra's canceled context into sqlite3 would call
		// sqlite3_interrupt while Vec1 is inside a native training loop.
		return sqlitevec.RunAcceleratorWorker(
			context.WithoutCancel(cmd.Context()), args[0], vector.GenerationID(generationID), threads,
		)
	},
}
