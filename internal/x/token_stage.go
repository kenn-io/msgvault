package x

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
)

// WithConsumedStage binds an installed credential to the exact staged grant it
// consumes. Persist this returned record atomically with the installed tokens.
// The bounded marker lives only in the private token file and survives refresh.
func (r TokenRecord) WithConsumedStage(stage TokenRecord) (TokenRecord, error) {
	digest, err := stageDigest(stage)
	if err != nil {
		return TokenRecord{}, err
	}
	r.consumedStage = digest
	return r, nil
}

// ConsumesStage identifies a leftover stage after interrupted publication or
// cleanup. Local generations, expiry and profile labels do not establish grant
// identity; matching uses the credential binding, secret data and granted scopes.
func (r TokenRecord) ConsumesStage(stage TokenRecord) (bool, error) {
	if r.consumedStage == "" {
		return false, nil
	}
	digest, err := stageDigest(stage)
	if err != nil {
		return false, err
	}
	return r.consumedStage == digest, nil
}

func stageDigest(stage TokenRecord) (string, error) {
	if err := validateRecord(stage); err != nil {
		return "", err
	}
	scopes := slices.Clone(stage.GrantedScopes)
	slices.Sort(scopes)
	binding := struct {
		OwnerID, ClientID       string
		ClientType              ClientType
		Scopes                  []string
		Access, Refresh, Secret string
	}{stage.OwnerID, stage.ClientID, stage.ClientType, scopes, stage.AccessToken(), stage.RefreshToken(), stage.ClientSecret()}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return "", errors.New("cannot bind X staged credential")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validConsumedStage(value string) bool {
	if value == "" {
		return true
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
