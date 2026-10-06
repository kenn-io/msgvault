package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func init() { rootCmd.AddCommand(newCredentialsCommand()) }

func newCredentialsCommand() *cobra.Command {
	command := &cobra.Command{Use: "credentials", Short: "Manage provider credentials on the daemon host"}
	var fromFile, endpoint string
	var fromStdin, asJSON bool
	set := &cobra.Command{
		Use: "set <id>", Short: "Store a provider key from an owner-only file or standard input", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := credentialsCommandConfig(cmd)
			if err != nil {
				return err
			}
			id := args[0]
			if err := validateHeadlessCredentialID(id); err != nil {
				return err
			}
			if (fromFile != "") == fromStdin {
				return usageErr(cmd, errors.New("provide exactly one of --from-file or --stdin"))
			}
			if id == providercredentials.PersonEnrichmentSuppressionID && cmd.Flags().Changed("endpoint") {
				return usageErr(cmd, errors.New("suppression keys have no endpoint"))
			}
			if id != providercredentials.PersonEnrichmentSuppressionID {
				if cmd.Flags().Changed("endpoint") {
					if endpoint == "" {
						return usageErr(cmd, errors.New("--endpoint must not be empty"))
					}
				} else {
					endpoint, err = configuredCredentialEndpoint(cfg, id)
					if err != nil {
						return err
					}
				}
			}
			var key string
			if fromStdin {
				key, err = providercredentials.ReadSecret(cmd.InOrStdin())
			} else {
				key, err = providercredentials.ReadSecretFile(fromFile)
			}
			if err != nil {
				return err
			}
			snapshot, err := providercredentials.Read(cfg.TokensDir())
			if err != nil {
				return err
			}
			if _, err := putHeadlessCredential(cfg, snapshot, id, endpoint, key); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved %s\n", id)
			if err != nil {
				return fmt.Errorf("write saved credential status: %w", err)
			}
			return nil
		},
	}
	set.Flags().StringVar(&fromFile, "from-file", "", "Read the key from an owner-only file")
	set.Flags().BoolVar(&fromStdin, "stdin", false, "Read the key from standard input")
	set.Flags().StringVar(&endpoint, "endpoint", "", "Bind the key to this provider URL (defaults to configured endpoint)")
	set.MarkFlagsMutuallyExclusive("from-file", "stdin")
	list := &cobra.Command{
		Use: "list", Short: "List stored IDs and bound origins without key values", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := credentialsCommandConfig(cmd)
			if err != nil {
				return err
			}
			snapshot, err := providercredentials.Read(cfg.TokensDir())
			if err != nil {
				return err
			}
			entries := snapshot.Metadata()
			if asJSON {
				writer := cmd.OutOrStdout()
				if err := json.MarshalWrite(writer, entries); err != nil {
					return fmt.Errorf("write credential metadata JSON: %w", err)
				}
				if _, err := io.WriteString(writer, "\n"); err != nil {
					return fmt.Errorf("write credential metadata JSON: %w", err)
				}
				return nil
			}
			for _, entry := range entries {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", entry.ID, entry.Origin); err != nil {
					return fmt.Errorf("write credential listing: %w", err)
				}
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "Print IDs and origins as JSON")
	importEnv := &cobra.Command{
		Use: "import-env", Short: "Import present configured provider keys without replacing stored keys", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := credentialsCommandConfig(cmd)
			if err != nil {
				return err
			}
			return importCredentialEnvironment(cmd, cfg)
		},
	}
	command.AddCommand(set, list, importEnv)
	return command
}

func credentialsCommandConfig(cmd *cobra.Command) (*config.Config, error) {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if isRemoteModeFor(state) {
		return nil, errors.New("credentials are stored on the daemon host; run there or pass --local")
	}
	return state.cfg, nil
}

func validateHeadlessCredentialID(id string) error {
	if strings.HasPrefix(id, providercredentials.PeopleProviderID("")) {
		return errors.New("people provider keys belong to named profiles; use msgvault person provider add --api-key-stdin or --credential-env")
	}
	if id == providercredentials.PersonEnrichmentSuppressionID {
		return nil
	}
	return providercredentials.ValidateID(id)
}

func configuredCredentialEndpoint(cfg *config.Config, id string) (string, error) {
	switch id {
	case providercredentials.VectorEmbeddingsID:
		return cfg.Vector.Embeddings.Endpoint, nil
	case providercredentials.VectorMultimodalID:
		return cfg.Vector.Multimodal.Endpoint, nil
	}
	for _, provider := range cfg.People.Enrichment.Providers {
		if providercredentials.PersonEnrichmentID(provider.Name) == id {
			provider.ApplyDefaults()
			return provider.CredentialEndpoint()
		}
	}
	return "", errors.New("provider is not configured; supply --endpoint")
}

func putHeadlessCredential(cfg *config.Config, snapshot providercredentials.Snapshot, id, endpoint, key string) (providercredentials.Snapshot, error) {
	if id == providercredentials.PersonEnrichmentSuppressionID {
		if _, err := personenrichment.NewSuppressionHasher([]byte(key)); err != nil {
			return providercredentials.Snapshot{}, fmt.Errorf("validate suppression key: %w", err)
		}
		return providercredentials.PutSuppression(cfg.TokensDir(), snapshot.ETag, key)
	}
	return providercredentials.Put(cfg.TokensDir(), snapshot.ETag, id, endpoint, key)
}

type credentialEnvironment struct{ id, environment string }

func configuredCredentialEnvironments(cfg *config.Config) []credentialEnvironment {
	entries := []credentialEnvironment{
		{providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.APIKeyEnv},
		{providercredentials.VectorMultimodalID, cfg.Vector.Multimodal.APIKeyEnv},
	}
	if cfg.People.Enrichment.SuppressionKeyEnv != providercredentials.StoredSuppressionEnvironment {
		entries = append(entries, credentialEnvironment{providercredentials.PersonEnrichmentSuppressionID, cfg.People.Enrichment.SuppressionKeyEnv})
	}
	for _, provider := range cfg.People.Enrichment.Providers {
		provider.ApplyDefaults()
		entries = append(entries, credentialEnvironment{providercredentials.PersonEnrichmentID(provider.Name), provider.APIKeyEnv})
	}
	return entries
}

func importCredentialEnvironment(cmd *cobra.Command, cfg *config.Config) error {
	snapshot, err := providercredentials.Read(cfg.TokensDir())
	if err != nil {
		return err
	}
	imported, kept := 0, 0
	for _, entry := range configuredCredentialEnvironments(cfg) {
		if snapshot.Stored(entry.id) {
			kept++
			continue
		}
		if entry.environment == "" {
			continue
		}
		key, ok := os.LookupEnv(entry.environment)
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		if len(key) > 64<<10 {
			return errors.New("provider environment credential exceeds 64 KiB limit")
		}
		var endpoint string
		if entry.id != providercredentials.PersonEnrichmentSuppressionID {
			endpoint, err = configuredCredentialEndpoint(cfg, entry.id)
			if err != nil {
				return err
			}
		}
		snapshot, err = putHeadlessCredential(cfg, snapshot, entry.id, endpoint, key)
		if err != nil {
			return err
		}
		imported++
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Imported %d credentials; kept %d existing credentials\n", imported, kept)
	if err != nil {
		return fmt.Errorf("write credential import summary: %w", err)
	}
	return nil
}
