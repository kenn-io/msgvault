// Package emailtags defines provider tag changes and their observable results.
package emailtags

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MessageTagChange adds or removes tags while preserving unrelated provider tags.
type MessageTagChange struct {
	Add     []string `json:"add,omitempty" maxItems:"100"`
	Remove  []string `json:"remove,omitempty" maxItems:"100"`
	Mailbox string   `json:"mailbox,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

type MessageTag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type MessageTagResult struct {
	ReceiptID         string   `json:"receipt_id,omitempty"`
	ReceiptStatus     string   `json:"receipt_status,omitempty"`
	IdempotencyKey    string   `json:"idempotency_key,omitempty"`
	MessageID         int64    `json:"message_id"`
	SourceID          int64    `json:"source_id"`
	Provider          string   `json:"provider"`
	Mailbox           string   `json:"mailbox,omitempty"`
	UIDValidity       uint32   `json:"uidvalidity,omitzero" format:"int64" maximum:"4294967295"`
	UID               uint32   `json:"uid,omitzero" format:"int64" maximum:"4294967295"`
	Flags             []string `json:"flags,omitempty"`
	Tags              []string `json:"tags"`
	Before            []string `json:"before"`
	AvailableTags     []Tag    `json:"available_tags"`
	CanCreateKeywords bool     `json:"can_create_keywords,omitempty"`
	DryRun            bool     `json:"dry_run"`
	Verified          bool     `json:"verified"`
}

// MessageTagError preserves the last observed result even when a write only partly applied.
type MessageTagError struct {
	Code    string  `json:"error"`
	Message string  `json:"message"`
	Result  *Result `json:"result,omitempty"`
	Cause   error   `json:"-"`
}

type Change = MessageTagChange
type Tag = MessageTag
type Result = MessageTagResult
type Error = MessageTagError

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Cause }
func Failure(code, message string, result *Result, cause error) *Error {
	return &Error{Code: code, Message: message, Result: result, Cause: cause}
}

func equal(a, b string, fold bool) bool {
	if fold {
		return strings.EqualFold(a, b)
	}
	return a == b
}
func Contains(values []string, want string, fold bool) bool {
	for _, v := range values {
		if equal(v, want, fold) {
			return true
		}
	}
	return false
}

func Normalize(change Change, fold bool) (Change, error) {
	if len(change.Add)+len(change.Remove) == 0 || len(change.Add) > 100 || len(change.Remove) > 100 {
		return Change{}, Failure("invalid_tag", "provide 1–100 add or remove tags", nil, nil)
	}
	normalize := func(input []string) ([]string, error) {
		out := make([]string, 0, len(input))
		for _, s := range input {
			if strings.TrimSpace(s) == "" || len(s) > 255 || !utf8.ValidString(s) {
				return nil, Failure("invalid_tag", "tags must be nonblank UTF-8 strings of at most 255 bytes", nil, nil)
			}
			if !Contains(out, s, fold) {
				out = append(out, s)
			}
		}
		return out, nil
	}
	var err error
	change.Add, err = normalize(change.Add)
	if err != nil {
		return Change{}, err
	}
	change.Remove, err = normalize(change.Remove)
	if err != nil {
		return Change{}, err
	}
	for _, s := range change.Add {
		if Contains(change.Remove, s, fold) {
			return Change{}, Failure("invalid_tag", fmt.Sprintf("tag %q occurs in both add and remove", s), nil, nil)
		}
	}
	return change, nil
}
func Delta(tags []string, change Change, fold bool) (add, remove []string) {
	for _, s := range change.Add {
		if !Contains(tags, s, fold) {
			add = append(add, s)
		}
	}
	for _, s := range change.Remove {
		if Contains(tags, s, fold) {
			remove = append(remove, s)
		}
	}
	return
}
func Project(tags []string, change Change, fold bool) []string {
	out := make([]string, 0, len(tags)+len(change.Add))
	for _, s := range tags {
		if !Contains(change.Remove, s, fold) {
			out = append(out, s)
		}
	}
	for _, s := range change.Add {
		if !Contains(out, s, fold) {
			out = append(out, s)
		}
	}
	return out
}
func Verify(tags []string, change Change, fold bool) bool {
	a, r := Delta(tags, change, fold)
	return len(a)+len(r) == 0
}
