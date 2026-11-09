package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

// Register ordinary Cobra commands so they work before the shared grammar
// migration, which can reuse this source group when it lands.
func registerMetaSourceProvider(name, short string, commands ...*cobra.Command) {
	var source *cobra.Command
	for _, command := range rootCmd.Commands() {
		if command.Name() == "source" {
			source = command
			break
		}
	}
	if source == nil {
		source = &cobra.Command{Use: "source", Short: "Add and sync provider sources"}
		rootCmd.AddCommand(source)
	}
	provider := &cobra.Command{Use: name, Short: short}
	provider.AddCommand(commands...)
	source.AddCommand(provider)
}

// ExitCode and Hint are the contracts consumed by the shared CLI diagnostic
// renderer. Keep the original cause available to errors.Is and errors.As.
type sourceCommandError struct {
	err  error
	code int
	hint string
}

func (e *sourceCommandError) Error() string { return e.err.Error() }
func (e *sourceCommandError) Unwrap() error { return e.err }
func (e *sourceCommandError) ExitCode() int { return e.code }
func (e *sourceCommandError) Hint() string  { return e.hint }

func metaSourceStateError(err error, provider string) error {
	switch {
	case errors.Is(err, store.ErrSourceNotFound):
		return &sourceCommandError{err: opserr.NotFound(err), code: 4, hint: fmt.Sprintf("register the profile with 'msgvault source %s add <identifier>'", provider)}
	case errors.Is(err, store.ErrSyncAlreadyActive):
		return &sourceCommandError{err: err, code: 6, hint: "wait for the active sync to finish, then retry"}
	case errors.Is(err, store.ErrSourceRetired):
		return &sourceCommandError{err: err, code: 6, hint: "select the active source profile; a retired source cannot sync"}
	default:
		return err
	}
}
