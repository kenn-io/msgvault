package cmd

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// ConfirmMode selects the prompt and validation for confirmDestructive.
type ConfirmMode int

const (
	// ConfirmModePermanent — destructive remote delete (rung 04).
	// Requires the literal word "delete" to confirm. Anything else,
	// including EOF, prints the verbatim cancellation message and
	// returns (false, nil).
	ConfirmModePermanent ConfirmMode = iota

	// ConfirmModeAllHidden — destructive local hard delete (rung 03)
	// targeting every hidden row. Accepts y/yes; n/no/EOF. EOF produces
	// the contract-naming error (cannot be skipped with --yes).
	ConfirmModeAllHidden

	// ConfirmModeYesNo — ordinary destructive prompt that may be
	// skipped with --yes by the caller. Accepts y/yes; n/no/EOF cancel
	// without an error so scripted/non-interactive use exits cleanly
	// when the prompt is reached unexpectedly.
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
			_, _ = fmt.Fprintln(w, "Cancelled. Drop --permanent to use trash deletion without elevated permissions.")
			return false, nil
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
			return false, fmt.Errorf(
				"no confirmation input (stdin closed); --all-hidden cannot be skipped with --yes",
			)
		}
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return answer == "y" || answer == "yes", nil

	case ConfirmModeYesNo:
		_, _ = fmt.Fprint(w, "Proceed? This is irreversible. [y/N]: ")
		scanner := bufio.NewScanner(r)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return false, fmt.Errorf("read confirmation: %w", err)
			}
			return false, nil
		}
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		return answer == "y" || answer == "yes", nil

	default:
		return false, fmt.Errorf("unknown ConfirmMode: %d", mode)
	}
}
