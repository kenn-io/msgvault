package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCandidateFormatsAreUniqueAndNonVisual(t *testing.T) {
	assert := assert.New(t)
	formats := CandidateFormats()
	ids := map[string]bool{}
	mediaTypes := map[string]bool{}
	for _, format := range formats {
		assert.NotEmpty(format.ID)
		assert.NotEmpty(format.Family)
		assert.NotEmpty(format.MediaType)
		assert.NotEmpty(format.UnitKind)
		assert.False(ids[format.ID], "duplicate format ID %s", format.ID)
		assert.False(mediaTypes[format.MediaType], "duplicate media type %s", format.MediaType)
		assert.NotEqual("image", format.Family)
		assert.NotEqual("audio", format.Family)
		assert.NotEqual("video", format.Family)
		ids[format.ID] = true
		mediaTypes[format.MediaType] = true
	}
	assert.Contains(ids, "pdf")
	assert.Contains(ids, "docx")
	assert.Contains(ids, "xlsx")
	assert.Contains(ids, "epub")
	assert.Contains(ids, "eml")
}

func TestRunCapabilityProbeKeepsCompleteSanitizedMatrix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	documents := candidateDocuments()
	processor := &fakeProbeProcessor{errors: map[string]error{
		"application/msword":       fmtPermanent(),
		"application/vnd.ms-excel": ErrTransientResponse,
	}}
	manifest, err := RunCapabilityProbe(t.Context(), processor, documents, ProbeConfig{
		ObservedAt: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		MaxPages:   500,
	})
	require.NoError(err)
	require.NoError(manifest.ValidateComplete())
	assert.Len(manifest.Results, len(CandidateFormats()))
	assert.Equal("0-499", processor.optionsByMediaType["application/pdf"].Pages)
	assert.Empty(processor.optionsByMediaType["application/vnd.openxmlformats-officedocument.wordprocessingml.document"].Pages)
	for mediaType, options := range processor.optionsByMediaType {
		assert.True(options.ExtractHeader, mediaType)
		assert.True(options.ExtractFooter, mediaType)
	}

	docResult := manifestResult(t, manifest, "doc")
	assert.Equal(ProbeStatusRejected, docResult.Status)
	assert.Equal("provider_4xx", docResult.ReasonCode)
	assert.Empty(docResult.ReturnedModel)
	xlsResult := manifestResult(t, manifest, "xls")
	assert.Equal(ProbeStatusFailed, xlsResult.Status)
	assert.Equal("transient_exhausted", xlsResult.ReasonCode)

	allowed, err := manifest.AllowedMediaTypes()
	require.NoError(err)
	assert.NotContains(allowed, "application/msword")
	assert.NotContains(allowed, "application/vnd.ms-excel")
	assert.Contains(allowed, "application/pdf")

	var encoded bytes.Buffer
	require.NoError(EncodeCapabilityManifest(&encoded, manifest))
	for _, document := range documents {
		assert.NotContains(encoded.String(), document.Path)
		assert.NotContains(encoded.String(), document.SHA256)
	}
	for _, candidate := range CandidateFormats() {
		sentinel, sentinelErr := ProbeFixtureSentinel(candidate.ID)
		require.NoError(sentinelErr)
		assert.NotContains(encoded.String(), sentinel, "extracted probe text must remain transient")
	}
	decoded, err := DecodeCapabilityManifest(bytes.NewReader(encoded.Bytes()))
	require.NoError(err)
	assert.Equal(manifest, decoded)
}

func TestCapabilityManifestRejectsPartialUnknownOrTamperedAuthority(t *testing.T) {
	require := require.New(t)
	manifest, err := RunCapabilityProbe(t.Context(), &fakeProbeProcessor{}, candidateDocuments(), ProbeConfig{
		ObservedAt: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), MaxPages: 10,
	})
	require.NoError(err)

	partial := manifest
	partial.Results = partial.Results[:len(partial.Results)-1]
	require.ErrorContains(partial.ValidateComplete(), "want")
	legacy := manifest
	legacy.SchemaVersion = 1
	legacy.ProbeFixtureContract = 0
	require.ErrorContains(legacy.ValidateComplete(), "schema must be 2")

	tampered := manifest
	tampered.Results = append([]CapabilityResult(nil), manifest.Results...)
	tampered.Results[0].Status = ProbeStatusPassed
	tampered.Results[0].RequestFingerprint = strings.Repeat("0", 64)
	require.ErrorContains(tampered.ValidateComplete(), "mismatched request fingerprint")

	invalidPass := manifest
	invalidPass.Results = append([]CapabilityResult(nil), manifest.Results...)
	invalidPass.Results[0].ReturnedModel = "unexpected-model"
	require.ErrorContains(invalidPass.ValidateComplete(), "incomplete")

	invalidFailure := manifest
	invalidFailure.Results = append([]CapabilityResult(nil), manifest.Results...)
	invalidFailure.Results[0].Status = ProbeStatusFailed
	invalidFailure.Results[0].ReasonCode = "synthetic_failure"
	invalidFailure.Results[0].ReturnedModel = ""
	invalidFailure.Results[0].UnitCount = 0
	invalidFailure.Results[0].UnitsProcessed = 1
	invalidFailure.Results[0].ProviderBytes = nil
	require.ErrorContains(invalidFailure.ValidateComplete(), "not scrubbed")

	var encoded bytes.Buffer
	require.NoError(EncodeCapabilityManifest(&encoded, manifest))
	withUnknown := bytes.Replace(encoded.Bytes(), []byte(`"schema_version": 2`), []byte(`"schema_version": 2, "secret": "x"`), 1)
	_, err = DecodeCapabilityManifest(bytes.NewReader(withUnknown))
	require.ErrorContains(err, "unknown field")
}

func TestRunCapabilityProbeRejectsFixtureMismatchBeforeProviderCall(t *testing.T) {
	documents := candidateDocuments()
	documents["docx"] = documents["pdf"]
	processor := &fakeProbeProcessor{}
	_, err := RunCapabilityProbe(t.Context(), processor, documents, ProbeConfig{MaxPages: 10})
	require.ErrorContains(t, err, "media type mismatch")
	assert.Zero(t, processor.calls)
}

func TestRunCapabilityProbeRequiresFormatSpecificSentinel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	docx, ok := CandidateFormatByID("docx")
	require.True(ok)
	processor := &fakeProbeProcessor{markdownByMediaType: map[string]string{
		docx.MediaType: "msgvault probe pdf cedar 7319",
	}}
	manifest, err := RunCapabilityProbe(t.Context(), processor, candidateDocuments(), ProbeConfig{
		ObservedAt: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC), MaxPages: 10,
	})
	require.NoError(err)
	result := manifestResult(t, manifest, "docx")
	assert.Equal(ProbeStatusFailed, result.Status)
	assert.Equal("sentinel_missing", result.ReasonCode)
	allowed, err := manifest.AllowedMediaTypes()
	require.NoError(err)
	assert.NotContains(allowed, docx.MediaType)

	sentinel, err := ProbeFixtureSentinel("pdf")
	require.NoError(err)
	assert.True(probeResponseContains([]Page{{Header: "**MSGVAULT**\nprobe PDF", Footer: "cedar-7319"}}, sentinel))
	_, err = ProbeFixtureSentinel("unknown")
	require.ErrorContains(err, "unknown")
}

func TestRunCapabilityProbeRejectsInvalidAuthorityBeforeProviderCalls(t *testing.T) {
	documents := candidateDocuments()
	tests := []struct {
		name      string
		processor *fakeProbeProcessor
		config    ProbeConfig
		want      string
	}{
		{name: "page limit", processor: &fakeProbeProcessor{}, config: ProbeConfig{MaxPages: 5_001}, want: "between 1 and 5000"},
		{name: "future date", processor: &fakeProbeProcessor{}, config: ProbeConfig{MaxPages: 10, ObservedAt: time.Now().UTC().Add(72 * time.Hour)}, want: "observation date"},
		{name: "target", processor: &fakeProbeProcessor{target: ProcessorTarget{Endpoint: "https://example.test/v1/ocr", Region: "eu", Model: defaultModel}}, config: ProbeConfig{MaxPages: 10}, want: "not pinned"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := RunCapabilityProbe(t.Context(), test.processor, documents, test.config)
			require.ErrorContains(t, err, test.want)
			assert.Zero(t, test.processor.calls)
		})
	}
}

func TestRunCapabilityProbePropagatesCancellationFromProcessor(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	processor := &cancelingProbeProcessor{cancel: cancel}
	_, err := RunCapabilityProbe(ctx, processor, candidateDocuments(), ProbeConfig{MaxPages: 10})
	require.ErrorIs(t, err, context.Canceled)
}

func candidateDocuments() map[string]Document {
	documents := make(map[string]Document, len(candidateFormats))
	for _, candidate := range candidateFormats {
		digest := sha256.Sum256([]byte("synthetic-" + candidate.ID))
		documents[candidate.ID] = Document{
			Path:      "/private/synthetic/" + candidate.ID,
			MediaType: candidate.MediaType,
			Size:      int64(len(candidate.ID)), SHA256: hex.EncodeToString(digest[:]),
		}
	}
	return documents
}

type fakeProbeProcessor struct {
	errors              map[string]error
	markdownByMediaType map[string]string
	optionsByMediaType  map[string]Options
	calls               int
	target              ProcessorTarget
}

type cancelingProbeProcessor struct {
	cancel context.CancelFunc
}

func (p *cancelingProbeProcessor) Target() ProcessorTarget {
	return ProcessorTarget{Endpoint: defaultEndpoint, Region: "eu", Model: defaultModel}
}

func (p *cancelingProbeProcessor) Process(context.Context, Document, Options) (Result, error) {
	p.cancel()
	return Result{}, context.Canceled
}

func (f *fakeProbeProcessor) Target() ProcessorTarget {
	if f.target.Endpoint == "" {
		return ProcessorTarget{Endpoint: defaultEndpoint, Region: "eu", Model: defaultModel}
	}
	return f.target
}

func (f *fakeProbeProcessor) Process(_ context.Context, document Document, options Options) (Result, error) {
	f.calls++
	if f.optionsByMediaType == nil {
		f.optionsByMediaType = map[string]Options{}
	}
	f.optionsByMediaType[document.MediaType] = options
	if err := f.errors[document.MediaType]; err != nil {
		return Result{}, err
	}
	providerBytes := document.Size
	candidate, ok := CandidateFormatByMediaType(document.MediaType)
	if !ok {
		return Result{}, errors.New("synthetic processor received unknown media type")
	}
	markdown, ok := f.markdownByMediaType[document.MediaType]
	if !ok {
		markdown, _ = ProbeFixtureSentinel(candidate.ID)
	}
	return Result{
		Model: defaultModel, Pages: []Page{{Index: 0, Markdown: markdown, indexPresent: true}},
		UsageInfo: &Usage{PagesProcessed: 1, DocSizeBytes: &providerBytes, pagesProcessedPresent: true},
	}, nil
}

func fmtPermanent() error {
	return errors.Join(errors.New("synthetic permanent rejection"), ErrPermanentResponse)
}

func manifestResult(t *testing.T, manifest CapabilityManifest, formatID string) CapabilityResult {
	t.Helper()
	for _, result := range manifest.Results {
		if result.FormatID == formatID {
			return result
		}
	}
	require.FailNow(t, "manifest result not found", formatID)
	return CapabilityResult{}
}
