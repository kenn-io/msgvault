package identityops

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/sourceops"
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

type SourceSelector = sourceops.Selector

type AddRequest struct {
	SourceSelector

	Identifier string `json:"identifier"`
	Signal     string `json:"signal"`
}

func (r *AddRequest) UnmarshalJSON(data []byte) error {
	type addRequest AddRequest
	var decoded addRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	set, err := jsonFieldPresent(data, "source_id")
	if err != nil {
		return err
	}
	decoded.SourceIDSet = set
	*r = AddRequest(decoded)
	return nil
}

type AddResult struct {
	Account    string `json:"account"`
	Identifier string `json:"identifier"`
	Signal     string `json:"signal"`
	Outcome    string `json:"outcome"`
	// CacheState reports whether the synchronous identity-dataset cache
	// refresh that follows the mutation succeeded ("ready") or failed
	// ("stale"). Set by the API layer after Add returns; empty when the
	// caller does not support cache refresh (e.g. non-daemon callers of
	// this package).
	CacheState string `json:"cache_state,omitempty" enum:"ready,stale"`
}

type RemoveRequest struct {
	SourceSelector

	Identifier string `json:"identifier"`
}

func (r *RemoveRequest) UnmarshalJSON(data []byte) error {
	type removeRequest RemoveRequest
	var decoded removeRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	set, err := jsonFieldPresent(data, "source_id")
	if err != nil {
		return err
	}
	decoded.SourceIDSet = set
	*r = RemoveRequest(decoded)
	return nil
}

func jsonFieldPresent(data []byte, field string) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, err
	}
	_, ok := fields[field]
	return ok, nil
}

type RemoveResult struct {
	Account    string `json:"account"`
	Identifier string `json:"identifier"`
	Removed    int64  `json:"removed"`
	NoIdentity bool   `json:"no_identity,omitempty"`
	// CacheState reports whether the synchronous identity-dataset cache
	// refresh that follows the mutation succeeded ("ready") or failed
	// ("stale"). Set by the API layer after Remove returns; empty when the
	// caller does not support cache refresh (e.g. non-daemon callers of
	// this package).
	CacheState string `json:"cache_state,omitempty" enum:"ready,stale"`
}

func Add(st Store, req AddRequest) (AddResult, error) {
	identifier := strings.TrimSpace(req.Identifier)
	if identifier == "" {
		return AddResult{}, opserr.Invalid(errors.New("identifier cannot be empty"))
	}
	if strings.Contains(req.Signal, ",") {
		return AddResult{}, opserr.Invalid(fmt.Errorf("signal names cannot contain commas: %q", req.Signal))
	}

	src, err := ResolveSource(st, req.SourceSelector)
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

	src, err := ResolveSource(st, req.SourceSelector)
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

func ResolveSource(st Store, selector SourceSelector) (*store.Source, error) {
	source, err := sourceops.ResolveExactOne(st, selector)
	if err == nil || opserr.KindOf(err) != opserr.KindNotFound ||
		selector.SourceIDSet || selector.SourceID != 0 || strings.TrimSpace(selector.Account) == "" {
		return source, err
	}

	account := strings.TrimSpace(selector.Account)
	_, collectionErr := st.GetCollectionByName(account)
	switch {
	case collectionErr == nil:
		return nil, opserr.Invalid(fmt.Errorf("%q is a collection, not an account", account))
	case errors.Is(collectionErr, store.ErrCollectionNotFound):
		return nil, err
	default:
		return nil, opserr.Internal(fmt.Errorf("look up collection %q: %w", account, collectionErr))
	}
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
