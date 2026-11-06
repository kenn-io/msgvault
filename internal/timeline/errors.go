package timeline

import "errors"

var (
	ErrBudgetTooSmall   = errors.New("output limit too small")
	ErrCursorExpired    = errors.New("cursor_expired")
	ErrWindowTooLarge   = errors.New("timeline window exceeds 31 days")
	ErrUnknownReference = errors.New("unknown timeline reference")
	ErrUndatedAnchor    = errors.New("undated timeline anchor")
	ErrInvalidRequest   = errors.New("invalid timeline request")
)

// ErrAmbiguousReference carries the bounded candidates for a non-unique name.
//
//nolint:errname // The shared timeline contract fixes this exported type name.
type ErrAmbiguousReference struct {
	Candidates []Candidate `json:"candidates"`
}

func (e *ErrAmbiguousReference) Error() string {
	return "more than one person matches; choose a match"
}
