package documentindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/msgvault/internal/fileutil"
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
	publishing        atomic.Bool
	renewedPublishing atomic.Bool
	publishStarted    chan struct{}
	releasePublish    <-chan struct{}
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
	ctx context.Context,
	publication store.DocumentExtractionPublication,
) error {
	c.publication = &publication
	if c.publishStarted != nil {
		c.publishing.Store(true)
		close(c.publishStarted)
		defer c.publishing.Store(false)
		select {
		case <-c.releasePublish:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.publishErr
}

func (c *workerCatalog) RenewDocumentExtractionClaim(
	_ context.Context,
	_ store.DocumentExtractionClaim,
	_ time.Time,
) error {
	c.renewals.Add(1)
	if c.publishing.Load() {
		c.renewedPublishing.Store(true)
	}
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
	result mistral.Result
	err    error
	calls  int
	cancel context.CancelFunc
	block  <-chan struct{}
}

func (p *workerProcessor) Process(
	ctx context.Context,
	_ *mistral.PreparedDocument,
	_ mistral.FormatAuthorization,
) (mistral.Result, error) {
	p.calls++
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

func TestMistralWorkerPublishesOnlyNormalizedDerivatives(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := mistraltest.MinimalPDF("worker test")
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	catalog := &workerCatalog{}
	opener := &workerOpener{content: content}
	processor := &workerProcessor{result: successfulWorkerResult(
		"# Invoice\n<script>private()</script>\nAmount **42**",
	)}
	worker := newTestMistralWorker(t, catalog, opener, processor)

	result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: digest, MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})
	require.NoError(err)
	require.NotNil(catalog.publication)
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
	require.NoError(err)
	assert.Len(entries, 1, "only the package reservation lock remains after publication")
}

func TestMistralWorkerRecordsSanitizedRetryWithoutPublishing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := mistraltest.MinimalPDF("worker test")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	processor := &workerProcessor{err: mistral.ErrTransientResponse}
	worker := newTestMistralWorker(t, catalog, &workerOpener{content: content}, processor)

	result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(err, mistral.ErrTransientResponse)
	assert.Equal(hex.EncodeToString(hash[:]), result.CanonicalBlobHash)
	assert.Equal("provider_transient", result.FailureReasonCode)
	require.NotNil(catalog.failure)
	assert.False(catalog.failure.Terminal)
	assert.Equal("provider_transient", catalog.failure.ReasonCode)
	assert.True(catalog.failure.RetryAt.After(time.Now().UTC()))
	assert.Nil(catalog.publication)
}

func TestClassifyDocumentExtractionFailurePreservesRetryablePreparation(t *testing.T) {
	terminal, reason := classifyDocumentExtractionFailure(
		errors.Join(errDocumentPreparation, mistral.ErrSpoolCapacity),
	)

	assert.False(t, terminal)
	assert.Equal(t, "spool_capacity_unavailable", reason)
}

func TestMistralWorkerReleasesClaimAfterRequestCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := mistraltest.MinimalPDF("worker test")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	ctx, cancel := context.WithCancel(t.Context())
	processor := &workerProcessor{err: context.Canceled, cancel: cancel}
	worker := newTestMistralWorker(t, catalog, &workerOpener{content: content}, processor)

	_, err := worker.ProcessCandidate(ctx, store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(err, context.Canceled)
	require.NotNil(catalog.failure)
	require.NoError(catalog.failureContextErr)
	assert.False(catalog.failure.Terminal)
	assert.Equal("provider_interrupted", catalog.failure.ReasonCode)
}

func TestMistralWorkerReleasesClaimAfterPublicationFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := mistraltest.MinimalPDF("worker test")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{publishErr: errors.New("synthetic publication failure")}
	worker := newTestMistralWorker(
		t, catalog, &workerOpener{content: content},
		&workerProcessor{result: successfulWorkerResult("searchable evidence")},
	)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(err, errDocumentPublication)
	require.NotNil(catalog.failure)
	assert.False(catalog.failure.Terminal)
	assert.Equal("publication_failed", catalog.failure.ReasonCode)
}

func TestMistralWorkerSuspendsLeaseRenewalDuringPublication(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := mistraltest.MinimalPDF("worker test")
	hash := sha256.Sum256(content)
	releasePublish := make(chan struct{})
	catalog := &workerCatalog{
		publishStarted: make(chan struct{}),
		releasePublish: releasePublish,
	}
	worker := newTestMistralWorker(
		t, catalog, &workerOpener{content: content},
		&workerProcessor{result: successfulWorkerResult("searchable evidence")},
	)
	worker.config.LeaseDuration = 15 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
			AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
			Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
		})
		done <- err
	}()
	select {
	case <-catalog.publishStarted:
	case <-time.After(time.Second):
		close(releasePublish)
		require.Fail("publication did not start")
	}
	time.Sleep(40 * time.Millisecond)
	close(releasePublish)
	require.NoError(<-done)
	assert.False(catalog.renewedPublishing.Load())
	assert.Positive(catalog.renewals.Load(), "the claim is renewed immediately before publication")
}

func TestMistralWorkerCancelsProcessingWhenLeaseRenewalFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := mistraltest.MinimalPDF("worker test")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{renewErr: errors.New("synthetic renewal failure")}
	processor := &workerProcessor{block: make(chan struct{})}
	worker := newTestMistralWorker(t, catalog, &workerOpener{content: content}, processor)
	worker.config.LeaseDuration = 15 * time.Millisecond

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(err, errDocumentLeaseRenewal)
	assert.Positive(catalog.renewals.Load())
	require.NotNil(catalog.failure)
	assert.False(catalog.failure.Terminal)
	assert.Equal("lease_renewal_failed", catalog.failure.ReasonCode)
}

func TestMistralWorkerRejectsUnboundedFormatBeforeReadingBytes(t *testing.T) {
	opener := &workerOpener{content: []byte("unused")}
	worker := newTestMistralWorker(t, &workerCatalog{}, opener, &workerProcessor{})

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: strings.Repeat("a", 64),
		MIMEType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		Size:     10, MessageType: "email",
	})
	require.ErrorContains(t, err, "lacks passing capability authority")
	assert.Zero(t, opener.opened)
}

func TestMistralWorkerRecordsOversizedCandidateBeforeReadingBytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	opener := &workerOpener{content: []byte("unused")}
	catalog := &workerCatalog{}
	worker := newTestMistralWorker(t, catalog, opener, &workerProcessor{})

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: strings.Repeat("a", 64),
		MIMEType: "application/pdf", Size: (1 << 20) + 1, MessageType: "email",
	})
	require.ErrorContains(err, "candidate size is outside configured bounds")
	assert.Zero(opener.opened)
	require.NotNil(catalog.failure)
	assert.True(catalog.failure.Terminal)
	assert.Equal("invalid_local_source", catalog.failure.ReasonCode)
}

func TestMistralWorkerClaimsBeforeWritingPrivateSpool(t *testing.T) {
	content := mistraltest.MinimalPDF("worker test")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{claimErr: store.ErrDocumentExtractionClaimed}
	worker := newTestMistralWorker(t, catalog, &workerOpener{content: content}, &workerProcessor{})

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(t, err, store.ErrDocumentExtractionClaimed)
	entries, readErr := os.ReadDir(worker.config.SpoolDirectory)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestMistralWorkerClassifiesCapabilityDrift(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := mistraltest.MinimalPDF("worker test")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	worker := newTestMistralWorker(
		t, catalog, &workerOpener{content: content},
		&workerProcessor{err: mistral.ErrCapabilityContract},
	)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(err, mistral.ErrCapabilityContract)
	require.NotNil(catalog.failure)
	assert.True(catalog.failure.Terminal)
	assert.Equal("provider_capability_changed", catalog.failure.ReasonCode)
}

func newTestMistralWorker(
	t *testing.T,
	catalog DocumentExtractionCatalog,
	opener DocumentAttachmentOpener,
	processor MistralProcessor,
) *MistralWorker {
	t.Helper()
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, fileutil.SecureMkdirAll(spoolDirectory, 0o700))
	policy := testMistralPolicy(t)
	worker, err := NewMistralWorker(catalog, opener, processor, MistralWorkerConfig{
		ProfileID: "profile-test", LeaseOwner: "worker-test", LeaseDuration: 30 * time.Minute,
		RetryDelay: 5 * time.Minute, SpoolDirectory: spoolDirectory,
		MaxSpoolBytes: 2 << 20, MinFreeBytes: 1,
		Policy: policy, CapabilityPolicy: testCapabilityManifest(t, policy),
	})
	require.NoError(t, err)
	return worker
}

func testMistralPolicy(t *testing.T) mistral.Policy {
	t.Helper()
	normalizePolicy, err := document.NewNormalizePolicy(1_000_000)
	require.NoError(t, err)
	policy, err := mistral.NewPolicy(mistral.PolicyConfig{
		Region: mistral.RegionEU, Model: mistral.DefaultModel,
		Retention: mistral.RetentionZDR, Training: mistral.TrainingOptedOut,
		MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 100,
		ExtractHeader: true, ExtractFooter: true, NormalizePolicy: normalizePolicy,
	})
	require.NoError(t, err)
	return policy
}

func testCapabilityManifest(t *testing.T, policy mistral.Policy) mistral.CapabilityManifest {
	t.Helper()
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(t, err)
	for index := range manifest.Results {
		manifest.Results[index].FixtureDigest = strings.Repeat("0", 16)
	}
	require.NoError(t, manifest.ValidateComplete())
	return manifest
}

func successfulWorkerResult(markdown string) mistral.Result {
	return mistral.Result{
		Document: document.SourceDocument{
			Family: "pdf", UnitKind: "page",
			Units: []document.SourceUnit{{Index: 0, Markdown: markdown}},
		},
		ReturnedModel: mistral.DefaultModel, UnitsProcessed: 1,
		Metrics: mistral.RequestMetrics{Requests: 1, Latency: time.Millisecond},
	}
}
