package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/msgvault/cmd/msgvault/cmd"
)

const (
	exitCodeInterrupted = 130 // 128 + SIGINT, mirrors shell convention
)

func main() {
	os.Exit(run())
}

func run() int {
	os.Args = cmd.LauncherArgs(os.Args)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := cmd.ExecuteContext(ctx); err != nil {
		if isSignalCanceled(ctx, err) {
			return exitCodeInterrupted
		}
		return cmd.ExitCode(err)
	}
	return 0
}

func isSignalCanceled(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) && ctx.Err() == context.Canceled
}
