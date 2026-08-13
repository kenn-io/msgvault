package documentindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/documentindex/mistral"
	"go.kenn.io/msgvault/internal/store"
)

type workerCatalog struct {
	claimInput        store.DocumentExtractionClaimInput
	publication       *store.DocumentExtractionPublication
	failure           *store.DocumentExtractionFailure
	failureContextErr error
	claimErr          error
	publishErr        error
	renewErr          error
	renewals          atomic.Int32
}

func (c *workerCatalog) ClaimDocumentExtraction(
	_ context.Context,
	input store.DocumentExtractionClaimInput,
) (store.DocumentExtractionClaim, error) {
	c.claimInput = input
	if c.claimErr != nil {
		return store.DocumentExtractionClaim{}, c.claimErr
	}
	return store.DocumentExtractionClaim{DocumentExtractionClaimInput: input, LeaseFence: 1}, nil
}

func (c *workerCatalog) PublishDocumentExtraction(
	_ context.Context,
	publication store.DocumentExtractionPublication,
) error {
	c.publication = &publication
	return c.publishErr
}

func (c *workerCatalog) RenewDocumentExtractionClaim(
	_ context.Context,
	_ store.DocumentExtractionClaim,
	_ time.Time,
) error {
	c.renewals.Add(1)
	return c.renewErr
}

func (c *workerCatalog) FailDocumentExtraction(
	ctx context.Context,
	failure store.DocumentExtractionFailure,
) error {
	c.failure = &failure
	c.failureContextErr = ctx.Err()
	return nil
}

type workerOpener struct {
	content []byte
	opened  int
}

func (o *workerOpener) OpenStream(context.Context, string) (io.ReadCloser, int64, error) {
	o.opened++
	return io.NopCloser(bytes.NewReader(o.content)), int64(len(o.content)), nil
}

type workerProcessor struct {
	result  mistral.Result
	err     error
	calls   int
	options mistral.Options
	cancel  context.CancelFunc
	block   <-chan struct{}
}

func (p *workerProcessor) Process(
	ctx context.Context,
	_ mistral.Document,
	options mistral.Options,
) (mistral.Result, error) {
	p.calls++
	p.options = options
	if p.cancel != nil {
		p.cancel()
	}
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return mistral.Result{}, ctx.Err()
		}
	}
	return p.result, p.err
}

func (p *workerProcessor) Target() mistral.ProcessorTarget {
	return mistral.ProcessorTarget{
		Endpoint: "https://api.mistral.ai/v1/ocr", Region: "eu", Model: "mistral-ocr-4-0",
	}
}

func TestMistralWorkerPublishesOnlyNormalizedDerivatives(t *testing.T) {
	assert := assert.New(t)
	content := []byte("%PDF-1.7\nsynthetic")
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	catalog := &workerCatalog{}
	opener := &workerOpener{content: content}
	processor := &workerProcessor{result: mistral.Result{
		Model: "mistral-ocr-4-0",
		Pages: []mistral.Page{{
			Index: 0, Markdown: "# Invoice\n<script>private()</script>\nAmount **42**",
		}},
		UsageInfo: &mistral.Usage{PagesProcessed: 1},
	}}
	worker := newTestMistralWorker(t, catalog, opener, processor, allPassingCapabilityManifest(t))

	result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: digest, MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})
	require.NoError(t, err)
	require.NotNil(t, catalog.publication)
	assert.True(processor.options.ExtractHeader)
	assert.True(processor.options.ExtractFooter)
	assert.Equal("0-99", processor.options.Pages)
	assert.True(catalog.claimInput.RequireNoHead)
	assert.Equal(int64(11), catalog.claimInput.SourceSequence)
	assert.Equal("# Invoice\nAmount 42", catalog.publication.Units[0].Text)
	assert.NotContains(catalog.publication.Units[0].Text, "private")
	assert.Equal([]string{"Invoice"}, catalog.publication.Chunks[0].HeadingPath)
	assert.Equal(1, catalog.publication.RequestCount)
	assert.Zero(catalog.publication.RetryCount)
	assert.Positive(catalog.publication.ProviderLatencyMS)
	assert.Equal(1, result.Units)
	assert.Equal(1, result.Chunks)
	assert.Nil(catalog.failure)

	entries, err := os.ReadDir(worker.config.SpoolDirectory)
	require.NoError(t, err)
	assert.Empty(entries, "verified request spool must be removed after publication")
}

func TestMistralWorkerRecordsSanitizedRetryWithoutPublishing(t *testing.T) {
	assert := assert.New(t)
	content := []byte("%PDF-1.7\nretry")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	processor := &workerProcessor{err: mistral.ErrTransientResponse}
	worker := newTestMistralWorker(
		t, catalog, &workerOpener{content: content}, processor, allPassingCapabilityManifest(t),
	)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(t, err, mistral.ErrTransientResponse)
	require.NotNil(t, catalog.failure)
	assert.False(catalog.failure.Terminal)
	assert.Equal("provider_transient", catalog.failure.ReasonCode)
	assert.True(catalog.failure.RetryAt.After(time.Now().UTC()))
	assert.Nil(catalog.publication)
}

func TestMistralWorkerReleasesClaimAfterRequestCancellation(t *testing.T) {
	content := []byte("%PDF-1.7\ncanceled")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	ctx, cancel := context.WithCancel(t.Context())
	processor := &workerProcessor{err: context.Canceled, cancel: cancel}
	worker := newTestMistralWorker(
		t, catalog, &workerOpener{content: content}, processor, allPassingCapabilityManifest(t),
	)

	_, err := worker.ProcessCandidate(ctx, store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, catalog.failure)
	require.NoError(t, catalog.failureContextErr, "claim cleanup must outlive the canceled request context")
	assert.False(t, catalog.failure.Terminal)
	assert.Equal(t, "provider_interrupted", catalog.failure.ReasonCode)
}

func TestMistralWorkerReleasesClaimAfterPublicationFailure(t *testing.T) {
	content := []byte("%PDF-1.7\npublication failure")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{publishErr: errors.New("synthetic publication failure")}
	processor := &workerProcessor{result: mistral.Result{
		Model:     "mistral-ocr-4-0",
		Pages:     []mistral.Page{{Index: 0, Markdown: "searchable evidence"}},
		UsageInfo: &mistral.Usage{PagesProcessed: 1},
	}}
	worker := newTestMistralWorker(
		t, catalog, &workerOpener{content: content}, processor, allPassingCapabilityManifest(t),
	)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(t, err, errDocumentPublication)
	require.NotNil(t, catalog.failure)
	assert.False(t, catalog.failure.Terminal)
	assert.Equal(t, "publication_failed", catalog.failure.ReasonCode)
}

func TestMistralWorkerCancelsProcessingWhenLeaseRenewalFails(t *testing.T) {
	content := []byte("%PDF-1.7\nrenewal failure")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{renewErr: errors.New("synthetic renewal failure")}
	processor := &workerProcessor{block: make(chan struct{})}
	worker := newTestMistralWorker(
		t, catalog, &workerOpener{content: content}, processor, allPassingCapabilityManifest(t),
	)
	worker.config.LeaseDuration = 15 * time.Millisecond

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(t, err, errDocumentLeaseRenewal)
	assert.Positive(t, catalog.renewals.Load())
	require.NotNil(t, catalog.failure)
	assert.False(t, catalog.failure.Terminal)
	assert.Equal(t, "lease_renewal_failed", catalog.failure.ReasonCode)
}

func TestMistralWorkerRejectsFormatWithoutPassingProbeBeforeReadingBytes(t *testing.T) {
	manifest := capabilityManifestWithFailure(t, "docx")
	opener := &workerOpener{content: []byte("unused")}
	worker := newTestMistralWorker(t, &workerCatalog{}, opener, &workerProcessor{}, manifest)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID:      1,
		CanonicalBlobHash: strings.Repeat("a", 64),
		MIMEType:          "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Size:              10, MessageType: "email",
	})
	require.ErrorContains(t, err, "lacks passing capability authority")
	assert.Zero(t, opener.opened)
}

func TestMistralWorkerClaimsBeforeWritingPrivateSpool(t *testing.T) {
	content := []byte("%PDF-1.7\nduplicate")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{claimErr: store.ErrDocumentExtractionClaimed}
	opener := &workerOpener{content: content}
	worker := newTestMistralWorker(t, catalog, opener, &workerProcessor{}, allPassingCapabilityManifest(t))

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(t, err, store.ErrDocumentExtractionClaimed)
	entries, readErr := os.ReadDir(worker.config.SpoolDirectory)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "a rejected claim must not reserve or write private spool bytes")
}

func TestMistralWorkerRejectsProviderOutputBeyondPageLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := []byte("%PDF-1.7\noversized output")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	processor := &workerProcessor{result: mistral.Result{
		Model: "mistral-ocr-4-0", Pages: make([]mistral.Page, 101),
		UsageInfo: &mistral.Usage{PagesProcessed: 101},
	}}
	worker := newTestMistralWorker(
		t, catalog, &workerOpener{content: content}, processor, allPassingCapabilityManifest(t),
	)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorContains(err, "returned 101 units, limit 100")
	require.NotNil(catalog.failure)
	assert.True(catalog.failure.Terminal)
	assert.Equal("invalid_provider_output", catalog.failure.ReasonCode)
	assert.Nil(catalog.publication)
}

func newTestMistralWorker(
	t *testing.T,
	catalog DocumentExtractionCatalog,
	opener DocumentAttachmentOpener,
	processor mistral.Processor,
	manifest mistral.CapabilityManifest,
) *MistralWorker {
	t.Helper()
	spoolDirectory := t.TempDir()
	require.NoError(t, os.Chmod(spoolDirectory, 0o700))
	worker, err := NewMistralWorker(catalog, opener, processor, MistralWorkerConfig{
		ProfileID: "profile-test", LeaseOwner: "worker-test", LeaseDuration: 30 * time.Minute,
		RetryDelay: 5 * time.Minute, SpoolDirectory: spoolDirectory,
		MaxFileBytes: 1 << 20, MaxSpoolBytes: 2 << 20, MinFreeBytes: 1, MaxPages: 100,
		NormalizePolicy: DefaultNormalizePolicy(1_000_000), CapabilityPolicy: manifest,
	})
	require.NoError(t, err)
	return worker
}

func allPassingCapabilityManifest(t *testing.T) mistral.CapabilityManifest {
	t.Helper()
	return runCapabilityManifest(t, "")
}

func capabilityManifestWithFailure(t *testing.T, failedID string) mistral.CapabilityManifest {
	t.Helper()
	return runCapabilityManifest(t, failedID)
}

func runCapabilityManifest(t *testing.T, failedID string) mistral.CapabilityManifest {
	t.Helper()
	processor := &capabilityProcessor{failedID: failedID}
	documents := make(map[string]mistral.Document)
	for _, format := range mistral.CandidateFormats() {
		documents[format.ID] = mistral.Document{
			MediaType: format.MediaType, Size: 1, SHA256: strings.Repeat("a", 64),
		}
	}
	manifest, err := mistral.RunCapabilityProbe(t.Context(), processor, documents, mistral.ProbeConfig{
		ObservedAt: time.Now().UTC(), MaxPages: 100,
	})
	require.NoError(t, err)
	return manifest
}

type capabilityProcessor struct {
	failedID string
	calls    int
}

func (p *capabilityProcessor) Process(
	_ context.Context,
	document mistral.Document,
	_ mistral.Options,
) (mistral.Result, error) {
	format, ok := mistral.CandidateFormatByMediaType(document.MediaType)
	if !ok {
		return mistral.Result{}, errors.New("unexpected media type")
	}
	if format.ID == p.failedID {
		return mistral.Result{}, mistral.ErrPermanentResponse
	}
	p.calls++
	bytes := document.Size
	sentinel, err := mistral.ProbeFixtureSentinel(format.ID)
	if err != nil {
		return mistral.Result{}, err
	}
	return mistral.Result{
		Model: "mistral-ocr-4-0", Pages: []mistral.Page{{Index: 0, Markdown: sentinel}},
		UsageInfo: &mistral.Usage{PagesProcessed: 1, DocSizeBytes: &bytes},
	}, nil
}

func (p *capabilityProcessor) Target() mistral.ProcessorTarget {
	return mistral.ProcessorTarget{
		Endpoint: "https://api.mistral.ai/v1/ocr", Region: "eu", Model: "mistral-ocr-4-0",
	}
}
