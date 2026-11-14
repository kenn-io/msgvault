package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/draftlint"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/textutil"
)

type draftLintOptions struct{ Strict, ValidateOnly bool }

func addDraftLintFlags(command *cobra.Command) {
	command.Flags().Bool("strict", false, "refuse writes on draft lint errors")
	command.Flags().Bool("validate-only", false, "validate the draft without writing")
}

func setDraftLintFlag(options *draftLintOptions, seen map[string]bool, name, value string, hasValue bool) error {
	const boolTrue = "true"
	if seen[name] || hasValue && value != boolTrue && value != "false" {
		return draftReplyError("invalid_args", fmt.Errorf("--%s accepts one boolean value", name))
	}
	seen[name] = true
	enabled := !hasValue || value == boolTrue
	switch name {
	case "strict":
		options.Strict = enabled
	case "validate-only":
		options.ValidateOnly = enabled
	}
	return nil
}

func emailDraftLintInput(kind, body string, draft imaplib.ReplyDraft) draftlint.Input {
	in := draftlint.Input{Kind: kind, Channel: "email", Body: body}
	for _, address := range draft.Parsed.To {
		in.To = append(in.To, draftMailbox(address.Email))
	}
	for _, address := range draft.Parsed.Cc {
		in.Cc = append(in.Cc, draftMailbox(address.Email))
	}
	for _, address := range draft.Parsed.Bcc {
		in.Bcc = append(in.Bcc, draftMailbox(address.Email))
	}
	return in
}

func draftMailbox(address string) string {
	return strings.TrimSuffix(strings.TrimPrefix((&mail.Address{Address: address}).String(), "<"), ">")
}

func emitDraftLintRefusal(emit func(api.CLIRunEvent) error, result draftlint.Result, asJSON bool) error {
	var data string
	if asJSON {
		encoded, err := json.Marshal(struct {
			Status   string              `json:"status"`
			Error    string              `json:"error"`
			Warnings []draftlint.Warning `json:"warnings"`
		}{"refused", "draft_lint_failed", result.Warnings})
		if err != nil {
			return err
		}
		data = string(encoded) + "\n"
	} else {
		data = "Refused.\n" + draftLintText(result)
	}
	return emit(api.CLIRunEvent{Type: cliStreamStderr, Data: data})
}
func draftLintText(result draftlint.Result) string {
	var b strings.Builder
	seen := make(map[[2]string]bool)
	for _, w := range result.Warnings {
		message := textutil.SanitizeTerminal(w.Message)
		key := [2]string{w.Severity, message}
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Fprintf(&b, "%s: %s\n", w.Severity, message)
	}
	return b.String()
}
func appendDraftLint(advisory *draftRecipientAdvisory, result draftlint.Result) {
	advisory.Warnings = append(append([]draftlint.Warning(nil), result.Warnings...), advisory.Warnings...)
}

// gateDraftLint is shared by create and edit paths before new provider writes.
func (a *storeAPIAdapter) gateDraftLint(in draftlint.Input, options draftLintOptions, asJSON bool, emit func(api.CLIRunEvent) error) (draftlint.Result, bool, error) {
	result := draftlint.Check(in)
	if options.ValidateOnly {
		return result, true, emitValidatedDraft(emit, result, in, asJSON)
	}
	if options.Strict && result.Blocking() {
		if err := emitDraftLintRefusal(emit, result, asJSON); err != nil {
			return result, true, draftReplyError("output_failed", err)
		}
		return result, true, draftReplyError("draft_lint_failed", errors.New("draft lint contains error findings"))
	}
	return result, false, nil
}
