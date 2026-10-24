package peoplesweep

import (
	"errors"

	"go.kenn.io/msgvault/internal/personfacts"
)

// MaxKnownEvidenceLimit bounds one known-evidence anchor read. The store may
// inspect a larger bounded page internally when unsupported evidence needs to
// be skipped, but never returns more than this many message IDs.
const MaxKnownEvidenceLimit = 2000

// KnownEvidenceRequest identifies the person and, optionally, one catalog
// target whose archive evidence should be used as a sweep anchor.
type KnownEvidenceRequest struct {
	PersonID   int64
	TargetKind personfacts.TargetKind
	TargetKey  string
	Limit      int
}

// Validate checks the request's boundedness and target scope. Target fields
// are deliberately validated as a pair so an empty target means an
// unscoped read, while a non-empty target always names one catalog target.
func (r KnownEvidenceRequest) Validate() error {
	if r.PersonID <= 0 {
		return errors.New("known evidence person id must be positive")
	}
	if r.Limit < 1 || r.Limit > MaxKnownEvidenceLimit {
		return errors.New("known evidence limit must be between 1 and 2000")
	}
	if (r.TargetKind == "") != (r.TargetKey == "") {
		return errors.New("known evidence target kind and key must be both set or both empty")
	}
	return nil
}
