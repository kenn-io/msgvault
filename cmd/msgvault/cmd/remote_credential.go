package cmd

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/remotecredential"
	"go.kenn.io/msgvault/pkg/credentialprovider"
)

// remoteCredentialSource asks the remote's credential helper for the read
// operations the CLI calls against a remote, limited to the configured
// delegation. A helper failure never falls back to an API key.
func remoteCredentialSource(cfg *config.Config) (*remotecredential.Source, error) {
	remote := cfg.Remote
	if remote.APIKey != "" || remote.APIKeyFile != "" || remote.APIKeyEnv != "" {
		return nil, errors.New("a remote with credential_provider cannot also set api_key, api_key_file, or api_key_env")
	}
	delegation := remote.Delegation
	if delegation == nil {
		return nil, errors.New("a remote with credential_provider needs a delegation table with archive_id, client_id, delegation_id, and source_ids")
	}
	ids := slices.Sorted(slices.Values(delegation.SourceIDs))
	sourceIDs := make([]string, len(ids))
	for i, id := range ids {
		sourceIDs[i] = strconv.FormatInt(id, 10)
	}
	return remotecredential.New(remote.CredentialProvider, credentialprovider.Scope{
		Endpoint:     remote.URL,
		ArchiveID:    delegation.ArchiveID,
		ClientID:     delegation.ClientID,
		DelegationID: delegation.DelegationID,
		SourceIDs:    sourceIDs,
		Operations:   credentialprovider.ReadOperations(),
	}, filepath.Join(cfg.HomeDir, "remote-credentials"))
}
