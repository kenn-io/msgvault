package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// ConfirmMode selects the prompt and validation for confirmDestructive.
type ConfirmMode int

const (
	// ConfirmModePermanent — destructive remote delete (rung 04).
	// Requires the literal word "delete" to confirm. Anything else,
	// prints the cancellation message and returns (false, nil). EOF is
	// an error because no confirmation was supplied.
	ConfirmModePermanent ConfirmMode = iota

	// ConfirmModeAllHidden — destructive local hard delete (rung 03)
	// targeting every hidden row. Accepts y/yes; n/no/EOF. EOF produces
	// the contract-naming error (cannot be skipped with --yes).
	ConfirmModeAllHidden

	// ConfirmModeYesNo — ordinary destructive prompt that may be
	// skipped with --yes by the caller. Accepts y/yes; explicit negative
	// answers cancel, while EOF is an error.
	ConfirmModeYesNo
)

// confirmDestructive prompts on the provided writer and reads a single
// line of input from the provided reader. Returns (true, nil) on
// confirmation, (false, nil) on cancellation, (_, err) on a contract
// violation that should fail the command (e.g. AllHidden EOF).
//
// The reader/writer split lets unit tests inject fixed input and
// inspect the prompt + cancellation messages without standing up the
// full cobra RunE harness.
func confirmDestructive(r io.Reader, w io.Writer, mode ConfirmMode) (bool, error) {
	switch mode {
	case ConfirmModePermanent:
		_, _ = fmt.Fprint(w, `Type "delete" to confirm permanent deletion (no recovery): `)
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return false, fmt.Errorf("read confirmation: %w", err)
			}
			return false, confirmationEOF(ConfirmModePermanent)
		}
		if strings.TrimSpace(scanner.Text()) != "delete" {
			_, _ = fmt.Fprintln(w, "Cancelled. Drop --permanent to use trash deletion without elevated permissions.")
			return false, nil
		}
		return true, nil

	case ConfirmModeAllHidden:
		_, _ = fmt.Fprint(w, "Proceed? This is irreversible. [y/N]: ")
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return false, fmt.Errorf("read confirmation: %w", err)
			}
			return false, confirmationEOF(ConfirmModeAllHidden)
		}
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return isYesAnswer(answer), nil

	case ConfirmModeYesNo:
		_, _ = fmt.Fprint(w, "Proceed? This is irreversible. [y/N]: ")
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return false, fmt.Errorf("read confirmation: %w", err)
			}
			return false, confirmationEOF(ConfirmModeYesNo)
		}
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return isYesAnswer(answer), nil

	default:
		return false, fmt.Errorf("unknown ConfirmMode: %d", mode)
	}
}

func confirmationEOF(mode ConfirmMode) error {
	message := "no confirmation input (stdin closed); pass --yes after reviewing the operation"
	hint := "review the operation first, then pass --yes to confirm without a prompt"
	switch mode {
	case ConfirmModePermanent:
		message = `no confirmation input (stdin closed); type "delete" to confirm permanent deletion`
		hint = `run in a terminal and type "delete"; --yes never skips permanent-deletion confirmation`
	case ConfirmModeAllHidden:
		message = "--all-hidden requires interactive confirmation; stdin is closed"
		hint = "run 'msgvault deletion purge-deduped --all-hidden' in a terminal and type y; --yes cannot bypass this prompt"
	}
	return &cliError{Kind: "usage", Code: "confirmation_required", Message: message, Hint: hint, ExitCode: 2, Err: io.EOF}
}

// confirmEmbed reads a y/N answer from the command input. Default is no.
func confirmEmbed(cmd *cobra.Command, prompt string) (bool, error) {
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt+"[y/N]: ")
	confirmed, err := readYesNoConfirmation(bufio.NewReader(cmd.InOrStdin()))
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	return confirmed, err
}

func isYesAnswer(answer string) bool {
	return answer == "y" || answer == "yes"
}

// readYesNoConfirmation accepts an explicit answer, including a final line without a newline.
func readYesNoConfirmation(reader *bufio.Reader) (bool, error) {
	response, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	if errors.Is(err, io.EOF) && strings.TrimSpace(response) == "" {
		return false, confirmationEOF(ConfirmModeYesNo)
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return isYesAnswer(response), nil
}
