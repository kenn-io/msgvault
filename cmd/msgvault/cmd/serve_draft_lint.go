package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/draftlint"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type draftLintOptions struct{ Strict, AllowPlaceholders, ValidateOnly bool }

func addDraftLintFlags(command *cobra.Command) {
	command.Flags().Bool("strict", false, "refuse writes on draft lint errors")
	command.Flags().Bool("allow-placeholders", false, "report unresolved placeholders as warnings")
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
	case "allow-placeholders":
		options.AllowPlaceholders = enabled
	case "validate-only":
		options.ValidateOnly = enabled
	}
	return nil
}

func emailDraftLintInput(kind, body string, draft imaplib.ReplyDraft) draftlint.Input {
	in := draftlint.Input{Kind: kind, Channel: "email", Subject: draft.Parsed.Subject, Body: body, BuiltSize: len(draft.Raw), AttachmentCount: len(draft.Parsed.Attachments)}
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

// MIME parsing decodes quoted local parts. Restore mailbox syntax for lint and
// envelope output while retaining bare addresses for ordinary mailboxes.
func draftMailbox(address string) string {
	return strings.TrimSuffix(strings.TrimPrefix((&mail.Address{Address: address}).String(), "<"), ">")
}
func draftMailboxKey(address string) string {
	if parsed, err := mail.ParseAddress(address); err == nil {
		address = parsed.Address
	}
	return store.NormalizeIdentifierForCompare(address)
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

// gateDraftLint is shared by create and edit paths and runs before locks,
// claims, provider calls, attachment catalog writes, or recovery transitions.
func (a *storeAPIAdapter) gateDraftLint(ctx context.Context, in draftlint.Input, options draftLintOptions, asJSON bool, emit func(api.CLIRunEvent) error) (draftlint.Result, bool, error) {
	in.AllowPlaceholders = options.AllowPlaceholders
	result := draftlint.Check(in)
	if err := ctx.Err(); err != nil {
		return result, true, draftReplyError("cancelled", err)
	}
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

func setDraftProvenanceFields(advisory *draftRecipientAdvisory, in draftlint.Input) {
	for i := range advisory.Warnings {
		w := &advisory.Warnings[i]
		if w.Code != draftlint.CodeRecipientProvenance {
			continue
		}
		for _, field := range []struct {
			name      string
			addresses []string
		}{{"to", in.To}, {"cc", in.Cc}, {"bcc", in.Bcc}} {
			for _, address := range field.addresses {
				if draftMailboxKey(address) == draftMailboxKey(w.Address) {
					w.Field = field.name
					break
				}
			}
			if w.Field != "" {
				break
			}
		}
	}
}
