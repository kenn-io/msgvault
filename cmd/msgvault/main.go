package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"go.kenn.io/msgvault/cmd/msgvault/cmd"
)

const (
	exitCodeError       = 1
	exitCodeInterrupted = 130 // 128 + SIGINT, mirrors shell convention
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) == 1 && filepath.Base(os.Args[0]) == "msgvault-muesli-hook" {
		os.Args = append([]string{os.Args[0], "muesli-hook"}, os.Args[1:]...)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := cmd.ExecuteContext(ctx); err != nil {
		if isSignalCanceled(ctx, err) {
			return exitCodeInterrupted
		}
		return exitCodeError
	}
	return 0
}

func isSignalCanceled(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) && ctx.Err() == context.Canceled
}
