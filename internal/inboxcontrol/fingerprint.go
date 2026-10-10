package inboxcontrol

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/emailtags"
)

// SemanticFingerprint binds provider state while excluding the time at which
// it was read. Tags and flags are sets; order alone does not invalidate a plan.
// Nil state markers remain distinct from observed false markers.
func SemanticFingerprint(state State) (string, error) {
	if state.Target != (Target{}) {
		if state.Source != (SourceIdentity{}) {
			return "", fmt.Errorf("%w: state has conflicting source bindings", ErrInvalid)
		}
		if err := state.Target.Validate(); err != nil {
			return "", err
		}
	} else if err := state.Source.Validate(); err != nil {
		return "", err
	}
	state.ObservedAt = time.Time{}
	state.Tags = canonicalSet(state.Tags, false)
	state.Flags = canonicalSet(state.Flags, false)
	state.Folders = slices.Clone(state.Folders)
	slices.SortFunc(state.Folders, func(a, b Folder) int {
		if n := strings.Compare(a.ID, b.ID); n != 0 {
			return n
		}
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		if n := strings.Compare(a.ParentID, b.ParentID); n != 0 {
			return n
		}
		return cmp.Compare(a.UIDValidity, b.UIDValidity)
	})
	return fingerprint(state)
}

// IntentFingerprint excludes transport/replay metadata and expected-state
// evidence. The receipt ledger compares this hash before checking freshness;
// a changed target, account, destination or action cannot reuse an old key.
func IntentFingerprint(request Request) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	var tags *emailtags.Change
	if request.Tags != nil {
		fold := request.Target.SourceType == sourceTypeIMAP
		normalized, err := emailtags.Normalize(*request.Tags, fold)
		if err != nil {
			return "", fmt.Errorf("%w: invalid tag delta", ErrInvalid)
		}
		normalized.Add = canonicalSet(normalized.Add, fold)
		normalized.Remove = canonicalSet(normalized.Remove, fold)
		tags = &normalized
	}
	return fingerprint(struct {
		Operation    Operation         `json:"operation"`
		Target       *Target           `json:"target,omitempty"`
		Source       *SourceIdentity   `json:"source,omitempty"`
		OriginFolder *Folder           `json:"origin_folder,omitempty"`
		Destination  *Folder           `json:"destination,omitempty"`
		Tags         *emailtags.Change `json:"tags,omitempty"`
		ReceiptID    string            `json:"receipt_id,omitempty"`
	}{request.Operation, request.Target, request.Source, request.OriginFolder, request.Destination, tags, request.ReceiptID})
}

func canonicalSet(input []string, fold bool) []string {
	output := slices.Clone(input)
	if fold {
		for i := range output {
			output[i] = strings.ToLower(output[i])
		}
	}
	slices.Sort(output)
	return slices.Compact(output)
}

func fingerprint(value any) (string, error) {
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("%w: cannot fingerprint operation", ErrInvalid)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
