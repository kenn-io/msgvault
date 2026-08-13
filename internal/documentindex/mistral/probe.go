package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxManifestBytes = int64(1 << 20)

type Processor interface {
	Process(ctx context.Context, document Document, options Options) (Result, error)
	Target() ProcessorTarget
}

type ProbeConfig struct {
	ObservedAt time.Time
	MaxPages   int
}

// RunCapabilityProbe executes the full candidate matrix serially. It returns
// only sanitized shape/accounting evidence; provider text and error bodies
// never enter the manifest.
func RunCapabilityProbe(
	ctx context.Context,
	processor Processor,
	documents map[string]Document,
	config ProbeConfig,
) (CapabilityManifest, error) {
	if processor == nil {
		return CapabilityManifest{}, errors.New("mistral capability probe requires a processor")
	}
	if config.ObservedAt.IsZero() {
		config.ObservedAt = time.Now().UTC()
	}
	if config.MaxPages <= 0 || config.MaxPages > 5_000 {
		return CapabilityManifest{}, errors.New("mistral capability probe requires a page limit between 1 and 5000")
	}
	observedOn := config.ObservedAt.UTC().Format(time.DateOnly)
	observed, observedErr := time.Parse(time.DateOnly, observedOn)
	if observedErr != nil || observed.After(time.Now().UTC().Add(24*time.Hour)) {
		return CapabilityManifest{}, errors.New("mistral capability probe has invalid observation date")
	}
	target := processor.Target()
	if target.Endpoint != defaultEndpoint || target.Region != "eu" || target.Model != defaultModel {
		return CapabilityManifest{}, errors.New("mistral capability probe processor target is not pinned")
	}
	if len(documents) != len(candidateFormats) {
		return CapabilityManifest{}, fmt.Errorf("mistral capability probe has %d fixtures, want %d", len(documents), len(candidateFormats))
	}
	for _, candidate := range candidateFormats {
		document, ok := documents[candidate.ID]
		if !ok {
			return CapabilityManifest{}, fmt.Errorf("mistral capability probe is missing fixture %q", candidate.ID)
		}
		if document.MediaType != candidate.MediaType {
			return CapabilityManifest{}, fmt.Errorf("mistral capability probe fixture %q media type mismatch", candidate.ID)
		}
		if _, err := shortFixtureDigest(document.SHA256); err != nil {
			return CapabilityManifest{}, fmt.Errorf("mistral capability probe fixture %q: %w", candidate.ID, err)
		}
	}

	manifest := CapabilityManifest{
		SchemaVersion: CapabilitySchemaVersion, ProbeFixtureContract: probeFixtureContract, ObservedOn: observedOn,
		Endpoint: target.Endpoint, Region: target.Region, RequestedModel: target.Model, MaxPages: config.MaxPages,
		Results: make([]CapabilityResult, 0, len(candidateFormats)),
	}
	for _, candidate := range candidateFormats {
		if err := ctx.Err(); err != nil {
			return CapabilityManifest{}, err
		}
		document := documents[candidate.ID]
		fixtureDigest, err := shortFixtureDigest(document.SHA256)
		if err != nil {
			return CapabilityManifest{}, fmt.Errorf("mistral capability probe fixture %q: %w", candidate.ID, err)
		}
		options := DefaultOptions()
		if candidate.Family == "pdf" {
			options.Pages = "0-" + strconv.Itoa(config.MaxPages-1)
		}
		result := CapabilityResult{
			FormatID: candidate.ID, Family: candidate.Family, MediaType: candidate.MediaType,
			UnitKind: candidate.UnitKind, FixtureDigest: fixtureDigest,
			RequestFingerprint: requestFingerprint(candidate, options),
		}
		response, processErr := processor.Process(ctx, document, options)
		if processErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return CapabilityManifest{}, ctxErr
			}
			result.Status, result.ReasonCode = classifyProbeError(processErr)
			manifest.Results = append(manifest.Results, result)
			continue
		}
		if response.UsageInfo == nil || len(response.Pages) == 0 || !hasExtractedText(response.Pages) {
			result.Status = ProbeStatusFailed
			result.ReasonCode = "empty_output"
			manifest.Results = append(manifest.Results, result)
			continue
		}
		sentinel, sentinelErr := ProbeFixtureSentinel(candidate.ID)
		if sentinelErr != nil {
			return CapabilityManifest{}, sentinelErr
		}
		if !probeResponseContains(response.Pages, sentinel) {
			result.Status = ProbeStatusFailed
			result.ReasonCode = "sentinel_missing"
			manifest.Results = append(manifest.Results, result)
			continue
		}
		result.Status = ProbeStatusPassed
		result.ReturnedModel = response.Model
		result.UnitCount = len(response.Pages)
		result.UnitsProcessed = response.UsageInfo.PagesProcessed
		result.ProviderBytes = response.UsageInfo.DocSizeBytes
		manifest.Results = append(manifest.Results, result)
	}
	if err := manifest.ValidateComplete(); err != nil {
		return CapabilityManifest{}, err
	}
	return manifest, nil
}

func classifyProbeError(err error) (string, string) {
	switch {
	case errors.Is(err, ErrPermanentResponse):
		return ProbeStatusRejected, "provider_4xx"
	case errors.Is(err, ErrTransientResponse):
		return ProbeStatusFailed, "transient_exhausted"
	case errors.Is(err, ErrResponseTooLarge):
		return ProbeStatusFailed, "response_too_large"
	default:
		return ProbeStatusFailed, "invalid_or_local_failure"
	}
}

func hasExtractedText(pages []Page) bool {
	for _, page := range pages {
		if page.Markdown != "" || page.Header != "" || page.Footer != "" {
			return true
		}
	}
	return false
}

func probeResponseContains(pages []Page, sentinel string) bool {
	var text strings.Builder
	for _, page := range pages {
		text.WriteString(page.Header)
		text.WriteByte(' ')
		text.WriteString(page.Markdown)
		text.WriteByte(' ')
		text.WriteString(page.Footer)
		text.WriteByte(' ')
	}
	return strings.Contains(normalizeProbeText(text.String()), normalizeProbeText(sentinel))
}

func normalizeProbeText(value string) string {
	var normalized strings.Builder
	space := true
	for _, char := range strings.ToLower(value) {
		if unicode.IsLetter(char) || unicode.IsNumber(char) {
			normalized.WriteRune(char)
			space = false
			continue
		}
		if !space {
			normalized.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(normalized.String())
}

func EncodeCapabilityManifest(writer io.Writer, manifest CapabilityManifest) error {
	if err := manifest.ValidateComplete(); err != nil {
		return err
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(manifest); err != nil {
		return fmt.Errorf("encode Mistral capability manifest: %w", err)
	}
	return nil
}

func DecodeCapabilityManifest(reader io.Reader) (CapabilityManifest, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxManifestBytes+1))
	if err != nil {
		return CapabilityManifest{}, fmt.Errorf("read Mistral capability manifest: %w", err)
	}
	if int64(len(data)) > maxManifestBytes {
		return CapabilityManifest{}, errors.New("mistral capability manifest is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest CapabilityManifest
	if err := decoder.Decode(&manifest); err != nil {
		return CapabilityManifest{}, fmt.Errorf("decode Mistral capability manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CapabilityManifest{}, errors.New("mistral capability manifest has trailing JSON")
	}
	if err := manifest.ValidateComplete(); err != nil {
		return CapabilityManifest{}, err
	}
	return manifest, nil
}
