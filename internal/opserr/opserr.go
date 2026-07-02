package opserr

import "errors"

// Kind classifies operation errors for transport-layer mapping.
type Kind int

const (
	KindInvalid Kind = iota + 1
	KindNotFound
	KindInternal
)

// Error wraps an operation error with a stable kind.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// KindOf returns the operation error kind, defaulting to internal errors.
func KindOf(err error) Kind {
	var opErr *Error
	if errors.As(err, &opErr) {
		return opErr.Kind
	}
	return KindInternal
}

// Wrap classifies err with kind.
func Wrap(kind Kind, err error) error {
	return &Error{Kind: kind, Err: err}
}

// Invalid classifies err as invalid user input.
func Invalid(err error) error {
	return Wrap(KindInvalid, err)
}

// NotFound classifies err as a missing operation target.
func NotFound(err error) error {
	return Wrap(KindNotFound, err)
}

// Internal classifies err as an internal operation failure.
func Internal(err error) error {
	return Wrap(KindInternal, err)
}
