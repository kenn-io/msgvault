// Package draftlint checks submitted draft text and computed envelopes.
package draftlint

type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
type Warning struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Span     *Span  `json:"span,omitempty"`
	Address  string `json:"address,omitempty"`
	Flag     string `json:"flag,omitempty"`
	Message  string `json:"message"`
}
type Input struct {
	Kind, Channel, Body string
	To, Cc, Bcc         []string
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
	CodeEmptyBody             = "empty_body"
	CodeUnresolvedPlaceholder = "unresolved_placeholder"
	CodeRecipientProvenance   = "recipient_provenance"
)
