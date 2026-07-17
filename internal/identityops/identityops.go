package identityops

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

const (
	AddOutcomeAdded            = "added"
	AddOutcomeAlreadyConfirmed = "already_confirmed"
	AddOutcomeAdditionalSignal = "additional_signal"
)

type Store interface {
	collectionops.AccountResolverStore
	ListAccountIdentities(sourceID int64) ([]store.AccountIdentity, error)
	AddAccountIdentity(sourceID int64, address, signal string) error
	RemoveAccountIdentity(sourceID int64, address string) (int64, error)
}

type AddRequest struct {
	Account    string `json:"account"`
	Identifier string `json:"identifier"`
	Signal     string `json:"signal"`
}

type AddResult struct {
	Account    string `json:"account"`
	Identifier string `json:"identifier"`
	Signal     string `json:"signal"`
	Outcome    string `json:"outcome"`
}

type RemoveRequest struct {
	Account    string `json:"account"`
	Identifier string `json:"identifier"`
}

type RemoveResult struct {
	Account    string `json:"account"`
	Identifier string `json:"identifier"`
	Removed    int64  `json:"removed"`
	NoIdentity bool   `json:"no_identity,omitempty"`
}

func Add(st Store, req AddRequest) (AddResult, error) {
	identifier := strings.TrimSpace(req.Identifier)
	if identifier == "" {
		return AddResult{}, opserr.Invalid(errors.New("identifier cannot be empty"))
	}
	if strings.Contains(req.Signal, ",") {
		return AddResult{}, opserr.Invalid(fmt.Errorf("signal names cannot contain commas: %q", req.Signal))
	}

	src, err := resolveAccountSource(st, req.Account)
	if err != nil {
		return AddResult{}, err
	}

	existing, err := st.ListAccountIdentities(src.ID)
	if err != nil {
		return AddResult{}, opserr.Internal(fmt.Errorf("list existing: %w", err))
	}
	var prevSignals []string
	for _, ai := range existing {
		if store.EqualIdentifier(ai.Address, identifier) {
			prevSignals = SplitSignalSet(ai.SourceSignal)
			break
		}
	}

	if err := st.AddAccountIdentity(src.ID, identifier, req.Signal); err != nil {
		return AddResult{}, opserr.Internal(fmt.Errorf("add identity: %w", err))
	}

	result := AddResult{
		Account:    src.Identifier,
		Identifier: identifier,
		Signal:     req.Signal,
		Outcome:    AddOutcomeAdded,
	}
	switch {
	case len(prevSignals) == 0:
	case slices.Contains(prevSignals, req.Signal):
		result.Outcome = AddOutcomeAlreadyConfirmed
	default:
		result.Outcome = AddOutcomeAdditionalSignal
	}
	return result, nil
}

func Remove(st Store, req RemoveRequest) (RemoveResult, error) {
	identifier := strings.TrimSpace(req.Identifier)
	if identifier == "" {
		return RemoveResult{}, opserr.Invalid(errors.New("identifier must not be empty"))
	}

	src, err := resolveAccountSource(st, req.Account)
	if err != nil {
		return RemoveResult{}, err
	}

	removed, err := st.RemoveAccountIdentity(src.ID, identifier)
	if err != nil {
		return RemoveResult{}, opserr.Internal(fmt.Errorf("remove identity: %w", err))
	}
	if removed == 0 {
		existing, listErr := st.ListAccountIdentities(src.ID)
		if listErr != nil {
			return RemoveResult{}, opserr.Internal(fmt.Errorf(
				"%s is not in %s's identity (and looking up the current set failed: %w)",
				identifier, src.Identifier, listErr))
		}
		have := make([]string, 0, len(existing))
		for _, ai := range existing {
			have = append(have, ai.Address)
		}
		if len(have) == 0 {
			return RemoveResult{}, opserr.NotFound(fmt.Errorf(
				"%s is not in %s's identity (no confirmed identifiers on this account)",
				identifier, src.Identifier))
		}
		return RemoveResult{}, opserr.NotFound(fmt.Errorf(
			"%s is not in %s's identity. Currently confirmed: %s",
			identifier, src.Identifier, strings.Join(have, ", ")))
	}

	result := RemoveResult{
		Account:    src.Identifier,
		Identifier: identifier,
		Removed:    removed,
	}
	rest, listErr := st.ListAccountIdentities(src.ID)
	if listErr == nil && len(rest) == 0 {
		result.NoIdentity = true
	}
	return result, nil
}

func resolveAccountSource(st Store, input string) (*store.Source, error) {
	if input == "" {
		return nil, opserr.Invalid(errors.New("account is required"))
	}

	scope, err := collectionops.ResolveAccount(st, input)
	if err != nil {
		return nil, err
	}
	if scope.Source == nil {
		return nil, opserr.Invalid(fmt.Errorf("no primary account source found for %q", input))
	}
	return scope.Source, nil
}

// SplitSignalSet parses a stored source_signal field into a sorted slice.
// Empty input returns an empty slice, and empty parts are filtered to mirror
// mergeSignalSet's producer-side normalization.
func SplitSignalSet(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
