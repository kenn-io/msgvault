package visual

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage"
)

// VoyageConfig assembles the docbank Voyage transport for this archive.
// Uploads fail closed without a validated capability manifest: an empty
// manifest constructs a provider whose every request is refused.
type VoyageConfig struct {
	APIKey    string
	Model     string
	Dimension int
	// Manifest is the operator's authenticated capability manifest. Every
	// upload authority is derived from it through the docbank policy.
	Manifest voyage.CapabilityManifest
	// Media carries the byte and pixel caps folded into the policy identity.
	Media MediaPolicy
	// Timeout bounds one provider attempt; zero uses the docbank default.
	Timeout time.Duration
	// HTTPClient overrides transport for tests.
	HTTPClient *http.Client
}

// VoyageProvider adapts go.kenn.io/docbank/document/voyage to this package's
// Provider interface: it converts assembled documents into docbank inputs,
// attaches the manifest-derived authorizations, and maps the docbank failure
// classification onto this package's provider sentinels.
type VoyageProvider struct {
	client         *voyage.Client
	policy         voyage.Policy
	authorizations []voyage.Authorization
	authorizedIDs  []string
	fingerprint    string
}

// NewVoyageProvider validates the pinned policy and derives upload authority
// from the manifest. Construction succeeds with zero authorized capabilities;
// requests then fail closed per input.
func NewVoyageProvider(config VoyageConfig) (*VoyageProvider, error) {
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{
		Model: config.Model, Dimension: config.Dimension,
		Media: config.Media.documentPolicy(),
	})
	if err != nil {
		return nil, fmt.Errorf("voyage policy: %w", err)
	}
	client, err := voyage.NewClient(policy, voyage.ClientConfig{
		APIKey: config.APIKey, Timeout: config.Timeout, HTTPClient: config.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("voyage client: %w", err)
	}
	provider := &VoyageProvider{client: client, policy: policy}
	if len(config.Manifest.Results) > 0 {
		authorizations, err := policy.AuthorizeAll(config.Manifest)
		if err != nil {
			return nil, fmt.Errorf("derive Voyage upload authority: %w", err)
		}
		provider.authorizations = authorizations
		for _, authorization := range authorizations {
			provider.authorizedIDs = append(provider.authorizedIDs, authorization.Capability().ID)
		}
		slices.Sort(provider.authorizedIDs)
		fingerprint, err := policy.Fingerprint(config.Manifest)
		if err != nil {
			return nil, fmt.Errorf("fingerprint Voyage policy: %w", err)
		}
		provider.fingerprint = fingerprint
	}
	return provider, nil
}

// AuthorizedCapabilities returns the sorted capability IDs with probed upload
// authority; eligibility policy folds them into rejection revisions.
func (p *VoyageProvider) AuthorizedCapabilities() []string {
	return slices.Clone(p.authorizedIDs)
}

// PolicyFingerprint returns the docbank policy identity consent records bind
// to, or empty when no manifest was supplied.
func (p *VoyageProvider) PolicyFingerprint() string { return p.fingerprint }

func (p *VoyageProvider) EmbedDocuments(
	ctx context.Context,
	documents []DocumentInput,
) ([]EmbeddingResult, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	inputs := make([]voyage.Input, len(documents))
	for index, document := range documents {
		input, err := documentVoyageInput(document)
		if err != nil {
			return nil, fmt.Errorf("build visual document %d: %w", index, err)
		}
		inputs[index] = input
	}
	result, err := p.client.EmbedDocuments(ctx, inputs, p.authorizations)
	if err != nil {
		return nil, mapVoyageError(ctx, err)
	}
	results := make([]EmbeddingResult, len(documents))
	for index := range documents {
		results[index] = EmbeddingResult{Owner: documents[index].Owner, Vector: result.Vectors[index]}
	}
	if len(results) > 0 {
		results[0].Usage = Usage{TotalTokens: result.Usage.TotalTokens, Available: result.Usage.Available}
	}
	return results, nil
}

func (p *VoyageProvider) EmbedQuery(
	ctx context.Context,
	query QueryInput,
) ([]float32, Usage, error) {
	parts := make([]voyage.Part, 0, 2)
	if text := strings.TrimSpace(query.Text); text != "" {
		parts = append(parts, voyage.Part{Text: text})
	}
	if query.Image != nil {
		part, err := voyageMediaPart(query.Image)
		if err != nil {
			return nil, Usage{}, err
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, Usage{}, errors.New("visual query requires text or image")
	}
	vector, usage, err := p.client.EmbedQuery(ctx, voyage.Input{Parts: parts}, p.authorizations...)
	if err != nil {
		return nil, Usage{}, mapVoyageError(ctx, err)
	}
	return vector, Usage{TotalTokens: usage.TotalTokens, Available: usage.Available}, nil
}

func documentVoyageInput(document DocumentInput) (voyage.Input, error) {
	if document.Owner.MessageID <= 0 || document.Owner.BlobHash == "" ||
		document.Owner.MediaInputKey == "" || document.Revision == "" {
		return voyage.Input{}, errors.New("invalid visual document identity")
	}
	parts := make([]voyage.Part, 0, len(document.Parts))
	for _, part := range document.Parts {
		switch {
		case part.Media != nil && part.Text != "":
			return voyage.Input{}, errors.New("visual input part cannot contain text and media")
		case part.Media != nil:
			converted, err := voyageMediaPart(part.Media)
			if err != nil {
				return voyage.Input{}, err
			}
			parts = append(parts, converted)
		case strings.TrimSpace(part.Text) != "":
			parts = append(parts, voyage.Part{Text: part.Text})
		default:
			return voyage.Input{}, errors.New("visual input part is empty")
		}
	}
	return voyage.Input{Parts: parts}, nil
}

// voyageMediaPart re-detects the media bytes so the metadata handed to the
// transport always describes them; the docbank client verifies the same
// invariant before serializing.
func voyageMediaPart(input *MediaInput) (voyage.Part, error) {
	if input == nil || len(input.Bytes) == 0 {
		return voyage.Part{}, errors.New("visual media bytes are required")
	}
	metadata, err := media.DetectBytes(input.Bytes, input.MIMEType)
	if err != nil {
		return voyage.Part{}, fmt.Errorf("detect visual media: %w", err)
	}
	return voyage.Part{Media: &voyage.Media{Metadata: metadata, Bytes: input.Bytes}}, nil
}

// mapVoyageError converts the docbank failure classification onto this
// package's provider sentinels, preserving Retry-After and status codes.
func mapVoyageError(ctx context.Context, err error) error {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return err
	}
	statusCode := 0
	if voyageErr, ok := errors.AsType[*voyage.ProviderError](err); ok {
		statusCode = voyageErr.StatusCode
	}
	switch {
	case errors.Is(err, voyage.ErrUnauthorized):
		return providerError(ErrProviderUnauthorized, statusCode, err)
	case errors.Is(err, voyage.ErrBatchTooLarge):
		return providerError(ErrProviderBatchTooLarge, statusCode, err)
	case errors.Is(err, voyage.ErrCapabilityContract):
		// Input this archive's manifest does not authorize is a terminal
		// rejection: retrying without a new probe cannot succeed.
		return providerError(ErrProviderRejected, statusCode, err)
	case errors.Is(err, voyage.ErrInvalidInput), errors.Is(err, voyage.ErrPermanentResponse):
		return providerError(ErrProviderRejected, statusCode, err)
	case errors.Is(err, voyage.ErrMalformedResponse):
		return providerMalformedError(err)
	case errors.Is(err, voyage.ErrTransientResponse):
		retryAfter, retrySet := voyage.RetryAfter(err)
		return providerRetryError(statusCode, retryAfter, retrySet, err)
	default:
		return providerRetryError(statusCode, 0, false, err)
	}
}
