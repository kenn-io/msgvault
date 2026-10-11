// Package emailtags defines provider tag changes and their observable results.
package emailtags

import (
	"fmt"
	"slices"
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
	MessageID         int64        `json:"message_id"`
	SourceID          int64        `json:"source_id"`
	Provider          string       `json:"provider"`
	Mailbox           string       `json:"mailbox,omitempty"`
	UIDValidity       uint32       `json:"uidvalidity,omitzero" format:"int64" maximum:"4294967295"`
	UID               uint32       `json:"uid,omitzero" format:"int64" maximum:"4294967295"`
	Flags             []string     `json:"flags,omitempty"`
	Tags              []string     `json:"tags"`
	Before            []string     `json:"before"`
	AvailableTags     []MessageTag `json:"available_tags"`
	CanCreateKeywords bool         `json:"can_create_keywords,omitempty"`
	DryRun            bool         `json:"dry_run"`
	Verified          bool         `json:"verified"`
}

// MessageTagError preserves the last observed result even when a write only partly applied.
type MessageTagError struct {
	Code    string            `json:"error"`
	Message string            `json:"message"`
	Result  *MessageTagResult `json:"result,omitempty"`
	Cause   error             `json:"-"`
}

func (e *MessageTagError) Error() string { return e.Code + ": " + e.Message }
func (e *MessageTagError) Unwrap() error { return e.Cause }
func Failure(code, message string, result *MessageTagResult, cause error) *MessageTagError {
	return &MessageTagError{Code: code, Message: message, Result: result, Cause: cause}
}

func Contains(values []string, want string, fold bool) bool {
	if fold {
		return slices.ContainsFunc(values, func(value string) bool { return strings.EqualFold(value, want) })
	}
	return slices.Contains(values, want)
}

func Normalize(change MessageTagChange, provider string) (MessageTagChange, error) {
	fold := provider == "imap" || provider == "msmail"
	if len(change.Add)+len(change.Remove) == 0 || len(change.Add) > 100 || len(change.Remove) > 100 {
		return MessageTagChange{}, Failure("invalid_tag", "provide 1–100 add or remove tags", nil, nil)
	}
	normalize := func(input []string) ([]string, error) {
		out := make([]string, 0, len(input))
		for _, s := range input {
			if strings.TrimSpace(s) == "" || !utf8.ValidString(s) {
				return nil, Failure("invalid_tag", "tags must be nonblank UTF-8 strings", nil, nil)
			}
			if provider == "imap" {
				if len(s) > 255 {
					return nil, Failure("invalid_tag", "IMAP tags allow at most 255 ASCII bytes", nil, nil)
				}
				for _, b := range []byte(s) {
					if b <= 0x20 || b >= 0x7f || strings.ContainsRune("(){%*\"\\]", rune(b)) {
						return nil, Failure("invalid_tag", "IMAP tags must be ASCII keyword atoms", nil, nil)
					}
				}
			}
			if provider == "msmail" && (utf8.RuneCountInString(s) > 255 || strings.Contains(s, ",")) {
				return nil, Failure("invalid_tag", "Microsoft categories allow at most 255 Unicode characters and exclude commas", nil, nil)
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
		return MessageTagChange{}, err
	}
	change.Remove, err = normalize(change.Remove)
	if err != nil {
		return MessageTagChange{}, err
	}
	for _, s := range change.Add {
		if Contains(change.Remove, s, fold) {
			return MessageTagChange{}, Failure("invalid_tag", fmt.Sprintf("tag %q occurs in both add and remove", s), nil, nil)
		}
	}
	return change, nil
}
func Delta(tags []string, change MessageTagChange, fold bool) (add, remove []string) {
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
func Project(tags []string, change MessageTagChange, fold bool) []string {
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
func Verify(tags []string, change MessageTagChange, fold bool) bool {
	a, r := Delta(tags, change, fold)
	return len(a)+len(r) == 0
}
