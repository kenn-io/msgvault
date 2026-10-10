// Package identitycontrol defines explicit, reviewed archive identity changes.
package identitycontrol

import (
	"errors"
	"fmt"
)

// Operation distinguishes graph edges from durable participant/person bindings.
type Operation string

const (
	OperationGraphLink    Operation = "graph-link"
	OperationGraphUnlink  Operation = "graph-unlink"
	OperationPersonLink   Operation = "person-link"
	OperationPersonUnlink Operation = "person-unlink"
	maxSafeIdentityID     int64     = 9_007_199_254_740_991
)

var ErrInvalidRequest = errors.New("invalid identity request")

// IdentityTarget names both endpoints. Graph operations use two participants;
// binding operations use a participant and a person, whose ID namespaces differ.
type IdentityTarget struct {
	ParticipantID      int64 `json:"participant_id" minimum:"1" maximum:"9007199254740991"`
	OtherParticipantID int64 `json:"other_participant_id,omitzero" minimum:"1" maximum:"9007199254740991"`
	PersonID           int64 `json:"person_id,omitzero" minimum:"1" maximum:"9007199254740991"`
}

type PreviewRequest struct {
	Operation Operation      `json:"operation" enum:"graph-link,graph-unlink,person-link,person-unlink"`
	Target    IdentityTarget `json:"target"`
}

// Validate requires one exact pair and IDs that survive JSON number transport.
// It validates shape only; the native service must separately authorize scope.
func (r PreviewRequest) Validate() error {
	switch r.Operation {
	case OperationGraphLink, OperationGraphUnlink:
		if r.Target.PersonID != 0 || !ValidID(r.Target.OtherParticipantID) ||
			r.Target.ParticipantID == r.Target.OtherParticipantID {
			return fmt.Errorf("%w: graph operations require two distinct participants", ErrInvalidRequest)
		}
	case OperationPersonLink, OperationPersonUnlink:
		if r.Target.OtherParticipantID != 0 || !ValidID(r.Target.PersonID) {
			return fmt.Errorf("%w: binding operations require one participant and one person", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unknown operation", ErrInvalidRequest)
	}
	if !ValidID(r.Target.ParticipantID) {
		return fmt.Errorf("%w: participant_id must be an exact positive JSON integer", ErrInvalidRequest)
	}
	return nil
}

// ValidID reports whether an archive identifier survives JSON number transport.
func ValidID(id int64) bool {
	return id > 0 && id <= maxSafeIdentityID
}

// Canonical orders an undirected graph pair while preserving binding direction.
func (t IdentityTarget) Canonical(operation Operation) IdentityTarget {
	if (operation == OperationGraphLink || operation == OperationGraphUnlink) &&
		t.ParticipantID > t.OtherParticipantID {
		t.ParticipantID, t.OtherParticipantID = t.OtherParticipantID, t.ParticipantID
	}
	return t
}
