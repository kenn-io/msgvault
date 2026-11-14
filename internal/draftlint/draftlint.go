// Package draftlint checks submitted draft text and computed envelopes.
package draftlint

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
type Result struct {
	Warnings []Warning
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
	CodeControlCharacters          = "control_characters"
	CodeAttachmentMentionedMissing = "attachment_mentioned_missing"
	CodeEmptySubject               = "empty_subject"
	CodeEmptyTo                    = "empty_to"
	CodeOversize                   = "oversize"
	CodeRecipientProvenance        = "recipient_provenance"
)
