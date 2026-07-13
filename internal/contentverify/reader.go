// Package contentverify verifies content-addressed bytes at consumption
// boundaries where a producer may discover corruption only after emitting a
// prefix.
package contentverify

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
)

var (
	// ErrMismatch reports content whose SHA-256 identity differs from the
	// requested hash.
	ErrMismatch = errors.New("content SHA-256 mismatch")
	// ErrVerificationIncomplete reports a reader closed before verified EOF.
	ErrVerificationIncomplete = errors.New("content verification incomplete")
)

// VerifyBytes verifies data against expected, which must be a SHA-256 hex
// digest. Hexadecimal case is accepted for compatibility with existing
// attachment callers.
func VerifyBytes(data []byte, expected string) error {
	want, err := parseExpected(expected)
	if err != nil {
		return err
	}
	got := sha256.Sum256(data)
	return compare(got, want, expected)
}

// NewReadCloser wraps src with verification against expected. A successful
// result requires reading through EOF; closing early reports
// ErrVerificationIncomplete.
func NewReadCloser(src io.ReadCloser, expected string) (io.ReadCloser, error) {
	if src == nil {
		return nil, errors.New("content verifier source is nil")
	}
	want, err := parseExpected(expected)
	if err != nil {
		return nil, err
	}
	return &readCloser{src: src, digest: sha256.New(), want: want, expected: expected}, nil
}

type readCloser struct {
	src      io.ReadCloser
	digest   hash.Hash
	want     [sha256.Size]byte
	expected string
	terminal error
	verified bool
	closed   bool
}

func (r *readCloser) Read(p []byte) (int, error) {
	if r.terminal != nil {
		return 0, r.terminal
	}
	n, err := r.src.Read(p)
	if n > 0 {
		_, _ = r.digest.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		var got [sha256.Size]byte
		copy(got[:], r.digest.Sum(nil))
		if verifyErr := compare(got, r.want, r.expected); verifyErr != nil {
			r.terminal = verifyErr
			return n, verifyErr
		}
		r.verified = true
		r.terminal = io.EOF
		return n, io.EOF
	}
	if err != nil {
		r.terminal = err
	}
	return n, err
}

func (r *readCloser) Close() error {
	if !r.closed {
		r.closed = true
		closeErr := r.src.Close()
		if r.verified {
			return closeErr
		}
		if r.terminal != nil && !errors.Is(r.terminal, io.EOF) {
			return errors.Join(r.terminal, closeErr)
		}
		return errors.Join(ErrVerificationIncomplete, closeErr)
	}
	if r.verified {
		return nil
	}
	if r.terminal != nil && !errors.Is(r.terminal, io.EOF) {
		return r.terminal
	}
	return ErrVerificationIncomplete
}

func parseExpected(expected string) ([sha256.Size]byte, error) {
	var want [sha256.Size]byte
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size {
		return want, fmt.Errorf("invalid expected SHA-256 %q", expected)
	}
	copy(want[:], decoded)
	return want, nil
}

func compare(got, want [sha256.Size]byte, expected string) error {
	if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
		return nil
	}
	return fmt.Errorf("%w: expected %s, got %x", ErrMismatch, expected, got)
}
