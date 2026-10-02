package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// visualRuntimePolicyGuard compares current operator files and live account
// resolution with the initialized authority, using the production policy and
// fingerprint functions. It never resolves credentials or contacts a provider.
func visualRuntimePolicyGuard(st *store.Store, cfg *config.Config, vf *visualFeatures) func(context.Context) error {
	path, homeDir := cfg.ConfigFilePath(), cfg.HomeDir
	generationFingerprint, policyFingerprint := vf.Generation.Fingerprint, vf.PolicyFingerprint
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := config.Load(path, homeDir)
		if err != nil || !current.Vector.Multimodal.Enabled {
			return api.ErrVisualPolicyChanged
		}
		vec, err := resolvedVectorConfig(st, current.Vector)
		if err != nil || vec.MultimodalGenerationFingerprint() != generationFingerprint {
			return api.ErrVisualPolicyChanged
		}
		provider, err := visualVoyageConfig(vec)
		if err != nil {
			return api.ErrVisualPolicyChanged
		}
		policy, err := provider.Policy()
		if err != nil {
			return api.ErrVisualPolicyChanged
		}
		fingerprint, err := policy.Fingerprint(provider.Manifest)
		if err != nil || fingerprint != policyFingerprint {
			return api.ErrVisualPolicyChanged
		}
		return nil
	}
}
