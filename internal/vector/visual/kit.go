package visual

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

// KitProvider sends visual inputs through Kit's embedding client.
type KitProvider struct {
	client      *embedclient.Client
	fingerprint string
}

func NewKitProvider(api, endpoint, model string, dimension int, key string, client *http.Client, fingerprint string) (*KitProvider, error) {
	roles := embedconfig.Roles{}
	if strings.HasPrefix(model, "gemini-embedding-2") {
		roles.QueryPrefix = "task: search result | query: "
	}
	c, err := embedclient.New(embedclient.Options{
		Model: embedconfig.Model{Name: model, Dimensions: dimension, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone, RequestDimensions: true}, Roles: roles,
		Deployment: embedconfig.Deployment{API: embedconfig.API(api), BaseURL: endpoint, TrustPrivateNetwork: true},
		Batch:      embedconfig.Batch{Items: 1}, Transport: embedconfig.Transport{Timeout: 45 * time.Second}, APIKey: key, HTTP: client,
	})
	if err != nil {
		return nil, err
	}
	return &KitProvider{client: c, fingerprint: fingerprint}, nil
}

func (p *KitProvider) PolicyFingerprint() string { return p.fingerprint }

func (p *KitProvider) AuthorizedCapabilities() []string {
	return []string{voyage.CapabilityImageJPEG, voyage.CapabilityImagePNG, voyage.CapabilityImageWebP, voyage.CapabilityVideoMP4}
}
func (p *KitProvider) EmbedDocuments(ctx context.Context, documents []DocumentInput) ([]EmbeddingResult, error) {
	inputs := make([]embedclient.Input, len(documents))
	for i, doc := range documents {
		for _, part := range doc.Parts {
			if part.Media != nil {
				inputs[i] = embedclient.Input{Role: embedconfig.RoleDocument, MediaType: part.Media.MIMEType, Data: part.Media.Bytes}
				break
			}
		}
	}
	vectors, err := p.client.EmbedInputs(ctx, inputs)
	if err != nil {
		return nil, kitError(err)
	}
	results := make([]EmbeddingResult, len(documents))
	for i, doc := range documents {
		results[i] = EmbeddingResult{Owner: doc.Owner, Vector: vectors[i]}
	}
	return results, nil
}
func (p *KitProvider) EmbedQuery(ctx context.Context, query QueryInput) ([]float32, Usage, error) {
	input := embedclient.Input{Role: embedconfig.RoleQuery, Text: query.Text}
	if query.Image != nil {
		input.Text = ""
		input.MediaType = query.Image.MIMEType
		input.Data = query.Image.Bytes
	}
	vectors, err := p.client.EmbedInputs(ctx, []embedclient.Input{input})
	if err != nil {
		return nil, Usage{}, kitError(err)
	}
	return vectors[0], Usage{}, nil
}
func kitError(err error) error {
	if errors.Is(err, embedclient.ErrInvalidVector) {
		return providerMalformedError(err)
	}
	if errors.Is(err, embedmodel.ErrUnsupportedContent) {
		return providerError(ErrProviderRejected, 0, err)
	}
	var api *embedclient.APIError
	if errors.As(err, &api) {
		switch {
		case api.Reason == embedclient.ReasonInvalidRequest:
			return providerError(ErrProviderRejected, api.StatusCode, err)
		case api.CredentialsRejected():
			return providerError(ErrProviderUnauthorized, api.StatusCode, err)
		case api.Retryable():
			return providerRetryError(api.StatusCode, api.RetryAfter, api.RetryAfter > 0, err)
		default:
			return providerError(ErrProviderRejected, api.StatusCode, err)
		}
	}
	return providerRetryError(0, 0, false, fmt.Errorf("kit embed: %w", err))
}
