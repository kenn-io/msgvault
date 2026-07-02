package accountops

import (
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/opserr"
)

// Store is the source surface needed by account mutation operations.
type Store interface {
	collectionops.AccountResolverStore
	UpdateSourceDisplayName(sourceID int64, displayName string) error
}

// UpdateRequest updates CLI-facing account settings.
type UpdateRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// UpdateResult is returned after updating CLI-facing account settings.
type UpdateResult struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// UpdateDisplayName updates one account's display name.
func UpdateDisplayName(st Store, req UpdateRequest) (UpdateResult, error) {
	if req.Email == "" {
		return UpdateResult{}, opserr.Invalid(errors.New("account email is required"))
	}
	if req.DisplayName == "" {
		return UpdateResult{}, opserr.Invalid(errors.New("display name is required"))
	}

	scope, err := collectionops.ResolveAccount(st, req.Email)
	if err != nil {
		return UpdateResult{}, err
	}
	if scope.Source == nil {
		return UpdateResult{}, opserr.Invalid(fmt.Errorf("no primary account source found for %q", req.Email))
	}

	if err := st.UpdateSourceDisplayName(scope.Source.ID, req.DisplayName); err != nil {
		return UpdateResult{}, opserr.Internal(fmt.Errorf("update display name: %w", err))
	}
	return UpdateResult{
		Email:       scope.Source.Identifier,
		DisplayName: req.DisplayName,
	}, nil
}
