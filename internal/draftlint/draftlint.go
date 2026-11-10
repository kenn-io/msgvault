// Package draftlint checks submitted draft text and computed envelopes.
package draftlint

import "context"

type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
type Warning struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Field    string `json:"field,omitempty"`
	Span     *Span  `json:"span,omitempty"`
	Excerpt  string `json:"excerpt,omitempty"`
	Address  string `json:"address,omitempty"`
	Flag     string `json:"flag,omitempty"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
}
type Input struct {
	Kind, Channel, Subject, Body string
	To, Cc, Bcc                  []string
	AttachmentCount, BuiltSize   int
	AllowPlaceholders            bool
}
type NameResolver interface {
	CuratedNames(context.Context, []string) (map[string][]string, error)
}
type Result struct {
	Warnings []Warning
	Skipped  []string
}

func (r Result) Blocking() bool {
	for _, w := range r.Warnings {
		if w.Severity == "error" {
			return true
		}
	}
	return false
}

const (
	CodeEmptyBody                  = "empty_body"
	CodeQuotedOnlyBody             = "quoted_only_body"
	CodeUnresolvedPlaceholder      = "unresolved_placeholder"
	CodeEmptyRecipients            = "empty_recipients"
	CodeInvalidAddress             = "invalid_address"
	CodeControlCharacters          = "control_characters"
	CodeAttachmentMentionedMissing = "attachment_mentioned_missing"
	CodeGreetingNameMismatch       = "greeting_name_mismatch"
	CodeEmptySubject               = "empty_subject"
	CodeEmptyTo                    = "empty_to"
	CodeSMSLength                  = "sms_length"
	CodeOversize                   = "oversize"
	CodeSensitiveContent           = "sensitive_content"
	CodeRecipientProvenance        = "recipient_provenance"
)
