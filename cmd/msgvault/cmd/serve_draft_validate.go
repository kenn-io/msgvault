package cmd

import (
	"encoding/json/v2"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/draftlint"
	"go.kenn.io/msgvault/internal/textutil"
)

type draftRecipients struct {
	To  []string `json:"to"`
	Cc  []string `json:"cc"`
	Bcc []string `json:"bcc"`
}
type draftValidateOutput struct {
	Status           string              `json:"status"`
	Valid            bool                `json:"valid"`
	Warnings         []draftlint.Warning `json:"warnings"`
	Recipients       *draftRecipients    `json:"recipients"`
	RecipientChanges []any               `json:"recipient_changes"`
}

func validatedDraftOutput(result draftlint.Result, recipients *draftRecipients) draftValidateOutput {
	return draftValidateOutput{Status: "validated", Valid: !result.Blocking(), Warnings: result.Warnings, Recipients: recipients, RecipientChanges: []any{}}
}
func emitValidatedDraft(emit func(api.CLIRunEvent) error, result draftlint.Result, in draftlint.Input, asJSON bool) error {
	var recipients *draftRecipients
	if in.Channel == "email" {
		recipients = &draftRecipients{To: in.To, Cc: in.Cc, Bcc: in.Bcc}
	}
	output := validatedDraftOutput(result, recipients)
	data := fmt.Sprintf("validated: valid=%t\n", output.Valid)
	if recipients != nil {
		data += fmt.Sprintf("to: %s\ncc: %s\nbcc: %s\n", textutil.SanitizeTerminal(strings.Join(in.To, ", ")), textutil.SanitizeTerminal(strings.Join(in.Cc, ", ")), textutil.SanitizeTerminal(strings.Join(in.Bcc, ", ")))
	}
	data += draftLintText(result)
	if asJSON {
		encoded, err := json.Marshal(output)
		if err != nil {
			return draftReplyError("output_failed", err)
		}
		data = string(encoded) + "\n"
	}
	if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: data}); err != nil {
		return draftReplyError("output_failed", err)
	}
	return nil
}
