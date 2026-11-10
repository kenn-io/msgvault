package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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

// Strip only the shared lint flags, leaving existing command validation intact.
func parseDraftLintFlags(args []string) ([]string, draftLintOptions, error) {
	var options draftLintOptions
	filtered := []string{args[0]}
	seen := map[string]bool{}
	key := false
	for i := 1; i < len(args); i++ {
		nameValue, ok := strings.CutPrefix(args[i], "--")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if ok && (name == "strict" || name == "allow-placeholders" || name == "validate-only") {
			if seen[name] || hasValue && value != "true" {
				return nil, options, draftReplyError("invalid_args", fmt.Errorf("--%s accepts one flag without a value", name))
			}
			seen[name] = true
			switch name {
			case "strict":
				options.Strict = true
			case "allow-placeholders":
				options.AllowPlaceholders = true
			case "validate-only":
				options.ValidateOnly = true
			}
			continue
		}
		if ok && name == "idempotency-key" {
			key = true
			if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
			continue
		}
		filtered = append(filtered, args[i])
		// Preserve flag values verbatim, even when submitted text starts with --.
		if ok && !hasValue {
			switch name {
			case "body", "subject", "from", "account", "source-id", "to", "cc", "bcc", "conversation", "reply-to", "revision", "log-level", "log-sql-slow-ms":
				if i+1 < len(args) {
					i++
					filtered = append(filtered, args[i])
				}
			}
		}
	}
	if key {
		if options.ValidateOnly {
			return nil, options, draftReplyError("validate_only_with_idempotency_key", errors.New("validation cannot use an idempotency key"))
		}
		return nil, options, draftReplyError("invalid_args", errors.New("unknown flag --idempotency-key"))
	}
	return filtered, options, nil
}

type draftNameResolver struct {
	store     *store.Store
	sourceIDs []int64
}

func (r draftNameResolver) CuratedNames(ctx context.Context, addresses []string) (map[string][]string, error) {
	return r.store.CuratedNamesForAddressesContext(ctx, addresses, r.sourceIDs)
}
func (a *storeAPIAdapter) lintDraft(ctx context.Context, in draftlint.Input, sourceID int64, grant *agentgrant.Grant) draftlint.Result {
	var resolver draftlint.NameResolver
	if grant == nil {
		resolver = draftNameResolver{store: a.store}
	} else if grant.HasPermission(agentgrant.PermissionMessageRead) || grant.HasPermission(agentgrant.PermissionSearchRead) {
		sources, err := a.store.ListSourcesContext(ctx, "")
		if err == nil {
			readable := make([]int64, 0)
			current := false
			for _, source := range sources {
				if grant.MatchesSource(draftSourceRef(source)) {
					readable = append(readable, source.ID)
					current = current || source.ID == sourceID
				}
			}
			if current {
				resolver = draftNameResolver{store: a.store, sourceIDs: readable}
			}
		}
	}
	return draftlint.Check(ctx, in, resolver)
}
func emailDraftLintInput(kind, body string, draft imaplib.ReplyDraft) draftlint.Input {
	in := draftlint.Input{Kind: kind, Channel: "email", Subject: draft.Parsed.Subject, Body: body, BuiltSize: len(draft.Raw), AttachmentCount: len(draft.Parsed.Attachments)}
	for _, address := range draft.Parsed.To {
		in.To = append(in.To, address.Email)
	}
	for _, address := range draft.Parsed.Cc {
		in.Cc = append(in.Cc, address.Email)
	}
	for _, address := range draft.Parsed.Bcc {
		in.Bcc = append(in.Bcc, address.Email)
	}
	return in
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
		data = "refused: draft_lint_failed\n" + draftLintText(result)
	}
	return emit(api.CLIRunEvent{Type: cliStreamStderr, Data: data})
}
func draftLintText(result draftlint.Result) string {
	var b strings.Builder
	for _, w := range result.Warnings {
		fmt.Fprintf(&b, "%s: %s: %s\n", w.Severity, w.Code, textutil.SanitizeTerminal(w.Message))
	}
	if len(result.Skipped) > 0 {
		fmt.Fprintf(&b, "lint skipped: %s\n", strings.Join(result.Skipped, ", "))
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
				if store.NormalizeIdentifierForCompare(address) == store.NormalizeIdentifierForCompare(w.Address) {
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
