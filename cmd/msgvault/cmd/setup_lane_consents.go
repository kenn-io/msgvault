package cmd

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

func combinedSetupConsent(purposes map[string]string) string {
	result := consentActive
	for _, state := range purposes {
		if state == consentUnknown {
			return consentUnknown
		}
		if state == consentStale {
			result = consentStale
		}
		if state == consentMissing && result == consentActive {
			result = consentMissing
		}
	}
	return result
}

func currentSetupConsent(exact bool, err error, anyActive func() (bool, error)) string {
	if err != nil {
		return consentUnknown
	}
	if exact {
		return consentActive
	}
	previous, err := anyActive()
	if err != nil {
		return consentUnknown
	}
	if previous {
		return consentStale
	}
	return consentMissing
}

func readSetupLaneConsents(ctx context.Context, cfg *config.Config, st *store.Store) *setupConsentState {
	if st == nil || cfg == nil {
		return nil
	}
	state := &setupConsentState{purposes: map[string]string{}}
	record := func(purpose, value string) { state.purposes[purpose] = value }
	record("documents", documentSetupConsentState(ctx, cfg, st))
	record("visual", visualSetupConsentState(ctx, cfg, st))
	record("person_inference", consentMissing)
	record("person_semantic", consentMissing)
	record("document_embedding", consentMissing)
	record("query_embedding", consentMissing)
	if cfg.People.Sweep.Enabled {
		profile, err := cfg.People.Sweep.Profile()
		value := consentUnknown
		if err == nil {
			active, err := st.HasActivePersonInferenceConsent(ctx, profile.Fingerprint)
			value = currentSetupConsent(active, err, func() (bool, error) { return st.HasAnyActiveLaneConsent(ctx, "person_inference") })
		}
		record("person_inference", value)
	}
	if cfg.Vector.Enabled && cfg.Vector.People.Enabled {
		profile, err := cfg.Vector.SemanticPersonEmbeddingProfile()
		value := consentUnknown
		if err == nil {
			active, err := st.HasActivePersonSemanticEmbeddingConsent(ctx, profile.Fingerprint)
			value = currentSetupConsent(active, err, func() (bool, error) { return st.HasAnyActiveLaneConsent(ctx, "person_semantic") })
		}
		record("person_semantic", value)
	}
	if cfg.Vector.Enabled && cfg.Attachments.Documents.Index.Embeddings.Enabled {
		target, targetErr := st.GetDocumentVectorTargetProfileID(ctx)
		for _, purpose := range []string{"document_embedding", "query_embedding"} {
			value := consentUnknown
			if errors.Is(targetErr, store.ErrDocumentVectorInvalidGenerationState) {
				value = currentSetupConsent(false, nil, func() (bool, error) { return st.HasAnyActiveLaneConsent(ctx, purpose) })
			} else if targetErr == nil {
				fingerprint, err := vectordocument.EgressFingerprint(target, cfg.Vector)
				if purpose == "query_embedding" {
					fingerprint, err = vectordocument.QueryEgressFingerprint(target, cfg.Vector)
				}
				if err == nil {
					consent, err := st.GetDocumentVectorConsent(ctx, fingerprint)
					value = currentSetupConsent(consent != nil && consent.Purpose == purpose, err, func() (bool, error) { return st.HasAnyActiveLaneConsent(ctx, purpose) })
				}
			}
			record(purpose, value)
		}
	}
	state.Documents = state.purposes["documents"] == consentActive
	state.Visual = state.purposes["visual"] == consentActive
	state.PersonInference = state.purposes["person_inference"] == consentActive
	state.PersonSemantic = state.purposes["person_semantic"] == consentActive
	state.DocumentEmbedding = state.purposes["document_embedding"] == consentActive
	state.QueryEmbedding = state.purposes["query_embedding"] == consentActive
	return state
}

func documentSetupConsentState(ctx context.Context, cfg *config.Config, st *store.Store) string {
	if !cfg.Attachments.Documents.Enabled {
		return consentMissing
	}
	manifest, err := loadDocumentCapabilityManifest(setupMistralManifestPath(cfg))
	if err != nil {
		return consentUnknown
	}
	_, profile, err := documentProfileForConfig(&cfg.Attachments.Documents, manifest)
	if err != nil {
		return consentUnknown
	}
	active, err := st.HasMatchingDocumentProviderConsent(ctx, profile)
	return currentSetupConsent(active, err, func() (bool, error) { return st.HasActiveDocumentProviderConsent(ctx) })
}

func visualSetupConsentState(ctx context.Context, cfg *config.Config, st *store.Store) string {
	if !cfg.Vector.Multimodal.Enabled {
		return consentMissing
	}
	vecCfg, err := resolvedVectorConfig(st, cfg.Vector)
	if err != nil {
		return consentUnknown
	}
	providerConfig, err := visualVoyageConfig(vecCfg)
	if err != nil {
		return consentUnknown
	}
	policy, err := providerConfig.Policy()
	if err != nil {
		return consentUnknown
	}
	policyFingerprint, err := policy.Fingerprint(providerConfig.Manifest)
	if err != nil {
		return consentUnknown
	}
	fingerprint := vecCfg.MultimodalGenerationFingerprint()
	result := consentMissing
	for _, read := range []func(context.Context) (store.VisualGeneration, error){st.ActiveVisualGeneration, st.BuildingVisualGeneration} {
		generation, err := read(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return consentUnknown
		}
		if !generation.Consented {
			continue
		}
		if generation.Fingerprint == fingerprint && generation.ConsentPolicyFingerprint == policyFingerprint {
			return consentActive
		}
		result = consentStale
	}
	return result
}
