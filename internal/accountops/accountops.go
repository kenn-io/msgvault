package accountops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/sourceops"
)

// Store is the source surface needed by account mutation operations.
type Store interface {
	sourceops.Store
	UpdateSourceDisplayName(sourceID int64, displayName string) error
}

// UpdateRequest updates CLI-facing account settings.
type UpdateRequest struct {
	Account     string `json:"account,omitempty"`
	Email       string `json:"email,omitempty"`
	SourceID    int64  `json:"source_id,omitempty"`
	SourceIDSet bool   `json:"-"`
	DisplayName string `json:"display_name"`
}

func (r *UpdateRequest) UnmarshalJSON(data []byte) error {
	type updateRequest UpdateRequest
	var decoded updateRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, decoded.SourceIDSet = fields["source_id"]
	*r = UpdateRequest(decoded)
	return nil
}

// UpdateResult is returned after updating CLI-facing account settings.
type UpdateResult struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// UpdateDisplayName updates one account's display name.
func UpdateDisplayName(st Store, req UpdateRequest) (UpdateResult, error) {
	account := strings.TrimSpace(req.Account)
	email := strings.TrimSpace(req.Email)
	if account != "" && email != "" && account != email {
		return UpdateResult{}, opserr.Invalid(errors.New("account and email selectors are mutually exclusive"))
	}
	if account == "" {
		account = email
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		return UpdateResult{}, opserr.Invalid(errors.New("display name is required"))
	}

	source, err := sourceops.ResolveExactOne(st, sourceops.Selector{
		Account:     account,
		SourceID:    req.SourceID,
		SourceIDSet: req.SourceIDSet,
	})
	if err != nil {
		return UpdateResult{}, err
	}

	if err := st.UpdateSourceDisplayName(source.ID, req.DisplayName); err != nil {
		return UpdateResult{}, opserr.Internal(fmt.Errorf("update display name: %w", err))
	}
	return UpdateResult{
		Email:       source.Identifier,
		DisplayName: req.DisplayName,
	}, nil
}
