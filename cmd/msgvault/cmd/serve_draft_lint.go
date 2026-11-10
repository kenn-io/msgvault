package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/agentgrant"
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
	if seen[name] || hasValue && value != "true" && value != "false" {
		return draftReplyError("invalid_args", fmt.Errorf("--%s accepts one boolean value", name))
	}
	seen[name] = true
	enabled := !hasValue || value == "true"
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

type draftNameResolver struct {
	adapter  *storeAPIAdapter
	sourceID int64
	grant    *agentgrant.Grant
}

func (r draftNameResolver) CuratedNames(ctx context.Context, addresses []string) ([]draftlint.Name, error) {
	var names []draftlint.Name
	var err error
	readable, status := r.adapter.draftReadableSources(ctx, r.sourceID, r.grant)
	switch status {
	case "checked":
		names, err = r.adapter.store.CuratedNamesForAddressesContext(ctx, addresses, readable)
	case "unavailable":
		err = errors.New("draft recipient source lookup failed")
	}
	if err != nil {
		logger := r.adapter.logger
		if logger == nil {
			logger = loggerFromContext(ctx)
		}
		logger.Warn("draft greeting name lookup failed", "error_class", "archive_read_failed")
	}
	return names, err
}
func (a *storeAPIAdapter) lintDraft(ctx context.Context, in draftlint.Input, sourceID int64, grant *agentgrant.Grant) draftlint.Result {
	var resolver draftlint.NameResolver
	if in.Channel == "email" {
		resolver = draftNameResolver{adapter: a, sourceID: sourceID, grant: grant}
	}
	return draftlint.Check(ctx, in, resolver)
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
	for _, w := range result.Warnings {
		fmt.Fprintf(&b, "%s: %s\n", w.Severity, textutil.SanitizeTerminal(w.Message))
	}
	if len(result.Skipped) > 0 {
		b.WriteString("Greeting check skipped.\n")
	}
	return b.String()
}
func appendDraftLint(advisory *draftRecipientAdvisory, result draftlint.Result) {
	advisory.Warnings = append(append([]draftlint.Warning(nil), result.Warnings...), advisory.Warnings...)
	advisory.LintSkipped = result.Skipped
}

// gateDraftLint is shared by create and edit paths and runs before locks,
// claims, provider calls, attachment catalog writes, or recovery transitions.
func (a *storeAPIAdapter) gateDraftLint(ctx context.Context, in draftlint.Input, sourceID int64, grant *agentgrant.Grant, options draftLintOptions, asJSON bool, emit func(api.CLIRunEvent) error) (draftlint.Result, bool, error) {
	in.AllowPlaceholders = options.AllowPlaceholders
	result := a.lintDraft(ctx, in, sourceID, grant)
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
