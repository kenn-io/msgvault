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
	"go.kenn.io/docbank/document/csvpdf"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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
	content  []byte
	opened   int
	closed   *atomic.Int32
	closeErr error
}

func (o *workerOpener) OpenStream(context.Context, string) (io.ReadCloser, int64, error) {
	o.opened++
	return &workerReadCloser{Reader: bytes.NewReader(o.content), closed: o.closed, closeErr: o.closeErr}, int64(len(o.content)), nil
}

type workerReadCloser struct {
	*bytes.Reader

	closed   *atomic.Int32
	closeErr error
}

func (r *workerReadCloser) Close() error {
	if r.closed != nil {
		r.closed.Add(1)
	}
	return r.closeErr
}

type workerProcessor struct {
	result            mistral.Result
	err               error
	calls             int
	cancel            context.CancelFunc
	block             <-chan struct{}
	preparedMediaType string
	preparedSHA256    string
	preparedSize      int64
}

func (p *workerProcessor) Process(
	ctx context.Context,
	prepared *mistral.PreparedDocument,
	_ mistral.FormatAuthorization,
) (mistral.Result, error) {
	p.calls++
	p.preparedMediaType = prepared.MediaType()
	p.preparedSHA256 = prepared.SHA256()
	p.preparedSize = prepared.Size()
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

func TestMistralWorkerConvertsCSVAndPublishesSourceBoundReceipt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := []byte("name,value\nalice,csv608sentinel\n")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	processor := &workerProcessor{result: successfulWorkerResult("csv608sentinel")}
	closed := &atomic.Int32{}
	worker := newCSVTestMistralWorker(t, catalog, &workerOpener{content: content, closed: closed}, processor)

	result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "text/csv",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})

	require.NoError(err)
	require.NotNil(catalog.publication)
	require.NotNil(catalog.publication.Conversion)
	assert.Equal(hex.EncodeToString(hash[:]), catalog.claimInput.CanonicalBlobHash)
	assert.Equal("original", catalog.claimInput.ExtractionInputKey)
	assert.Equal("text/csv", catalog.publication.OccurrenceMIMEType)
	assert.Equal("application/pdf", catalog.publication.Conversion.ProviderMediaType)
	assert.Equal(catalog.publication.Conversion.PDFSHA256, processor.preparedSHA256)
	assert.Equal(catalog.publication.Conversion.PDFBytes, processor.preparedSize)
	assert.Equal("application/pdf", processor.preparedMediaType)
	assert.Equal(hex.EncodeToString(hash[:]), catalog.publication.Conversion.SourceSHA256)
	assert.Equal(int64(len(content)), catalog.publication.Conversion.SourceBytes)
	assert.Equal("original", catalog.publication.ExtractionInputKey)
	assert.Equal(1, result.Units)
	assert.Equal(int32(1), closed.Load())
}

func TestMistralWorkerConvertsCSVAndPublishesThroughStore(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	content := []byte("name,value\nalice,csv608store\n")
	hashBytes := sha256.Sum256(content)
	hash := hex.EncodeToString(hashBytes[:])
	profile := store.DocumentExtractionProfile{
		ID: "profile-test", Fingerprint: strings.Repeat("a", 64),
		Provider: "mistral", Endpoint: "https://api.mistral.ai/v1/ocr",
		Region: "eu", Model: "mistral-ocr-4-0", RetentionPosture: "standard",
		TrainingPosture: "opted-out", AllowedMediaTypes: []string{"text/csv", "application/pdf"},
		PolicyJSON: []byte(`{"policy":1}`),
	}
	_, err := f.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))
	messageID := f.CreateMessage("document-worker-store")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "data.csv", MIMEType: "text/csv", Size: int64(len(content)),
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:1",
	}))
	var attachmentID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT id FROM attachments WHERE message_id = ? AND content_hash = ?`), messageID, hash).Scan(&attachmentID))
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
	require.NoError(err)
	require.True(eligible)

	processor := &workerProcessor{result: successfulWorkerResult("csv608store")}
	worker := newCSVTestMistralWorker(t, f.Store, &workerOpener{content: content}, processor)
	result, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: attachmentID, CanonicalBlobHash: hash, MIMEType: "text/csv",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})

	require.NoError(err)
	assert.Equal(1, result.Units)
	assert.Equal("application/pdf", processor.preparedMediaType)
	var providerMedia string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT provider_media_type FROM document_extraction_conversions
		WHERE extraction_id = ?`), result.ExtractionID).Scan(&providerMedia))
	assert.Equal("application/pdf", providerMedia)
	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "csv608store"})
	require.NoError(err)
	require.Len(response.Results, 1)
	assert.Equal(hash, response.Results[0].CanonicalBlobHash)
	assert.Equal("text/csv", response.Results[0].MIMEType)
}

func TestMistralWorkerSendsDirectPDFWithoutConversion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := mistraltest.MinimalPDF("direct PDF")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	processor := &workerProcessor{result: successfulWorkerResult("direct PDF")}
	worker := newTestMistralWorker(t, catalog, &workerOpener{content: content}, processor)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})

	require.NoError(err)
	require.NotNil(catalog.publication)
	assert.Equal("application/pdf", processor.preparedMediaType)
	assert.Equal(hex.EncodeToString(hash[:]), processor.preparedSHA256)
	assert.Equal(int64(len(content)), processor.preparedSize)
	assert.Nil(catalog.publication.Conversion)
}

func TestMistralWorkerPersistsCSVReceiptOnProviderFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := []byte("name,value\nalice,csv608failure\n")
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	worker := newCSVTestMistralWorker(t, catalog, &workerOpener{content: content}, &workerProcessor{err: mistral.ErrTransientResponse})

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "text/csv",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})

	require.ErrorIs(err, mistral.ErrTransientResponse)
	require.NotNil(catalog.failure)
	require.NotNil(catalog.failure.Conversion)
	assert.Equal(hex.EncodeToString(hash[:]), catalog.failure.Conversion.SourceSHA256)
	assert.Equal("application/pdf", catalog.failure.Conversion.ProviderMediaType)
}

func TestMistralWorkerClassifiesMalformedCSVAsInvalidLocalSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	catalog := &workerCatalog{}
	content := []byte("name,value\n\"unterminated\n")
	hash := sha256.Sum256(content)
	processor := &workerProcessor{result: successfulWorkerResult("unreachable")}
	closed := &atomic.Int32{}
	worker := newCSVTestMistralWorker(t, catalog, &workerOpener{content: content, closed: closed}, processor)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "text/csv",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})

	require.ErrorContains(err, "CSV source has invalid record syntax")
	require.NotNil(catalog.failure)
	assert.Equal("invalid_local_source", catalog.failure.ReasonCode)
	assert.Zero(processor.calls)
	assert.Equal(int32(1), closed.Load())
}

func TestMistralWorkerClosesCSVSourceWhenMetadataIsInvalid(t *testing.T) {
	require := require.New(t)
	closed := &atomic.Int32{}
	catalog := &workerCatalog{}
	content := []byte("name,value\nalice,closed\n")
	hash := sha256.Sum256(content)
	worker := newCSVTestMistralWorker(t, catalog, &workerOpener{content: content, closed: closed}, &workerProcessor{})
	worker.formats["text/csv; charset=utf-8"] = worker.formats["text/csv"]

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "text/csv; charset=utf-8",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})

	require.ErrorContains(err, "OCR source media type must be canonical")
	require.Equal(int32(1), closed.Load())
}

func TestMistralWorkerRecordsCSVSourceCloseFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	closed := &atomic.Int32{}
	catalog := &workerCatalog{}
	content := []byte("name,value\nalice,close-error\n")
	hash := sha256.Sum256(content)
	processor := &workerProcessor{result: successfulWorkerResult("unreachable")}
	worker := newCSVTestMistralWorker(t, catalog, &workerOpener{content: content, closed: closed, closeErr: errors.New("synthetic source close failure")}, processor)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 7, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "text/csv",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 11,
	})

	require.ErrorContains(err, "synthetic source close failure")
	require.NotNil(catalog.failure)
	assert.Equal("invalid_local_source", catalog.failure.ReasonCode)
	assert.Zero(processor.calls)
	assert.Equal(int32(1), closed.Load())
}

func newCSVTestMistralWorker(
	t *testing.T, catalog DocumentExtractionCatalog, opener DocumentAttachmentOpener, processor MistralProcessor,
) *MistralWorker {
	t.Helper()
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, fileutil.SecureMkdirAll(spoolDirectory, 0o700))
	policy := testMistralPolicy(t)
	manifest := testCapabilityManifest(t, policy)
	pdfFormat, found := mistral.CandidateFormatByID("pdf")
	require.True(t, found)
	authorization, err := policy.Authorize(manifest, pdfFormat.ID)
	require.NoError(t, err)
	csvPolicy, err := csvpdf.NewPolicy(csvpdf.DefaultLimits())
	require.NoError(t, err)
	worker, err := NewMistralWorker(catalog, opener, processor, MistralWorkerConfig{
		ProfileID: "profile-test", LeaseOwner: "worker-test", LeaseDuration: 30 * time.Minute,
		RetryDelay: 5 * time.Minute, SpoolDirectory: spoolDirectory,
		MaxSpoolBytes: 2 << 20, MinFreeBytes: 1, Policy: policy, CapabilityPolicy: manifest,
		InputPolicy: ResolvedInputPolicy{Routes: map[string]InputRoute{
			"text/csv": {Format: pdfFormat, Authorization: authorization, Conversion: &csvPolicy},
		}},
	})
	require.NoError(t, err)
	return worker
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
		InputPolicy: testPDFInputPolicy(t, policy),
	})
	require.NoError(t, err)
	return worker
}

func testPDFInputPolicy(t *testing.T, policy mistral.Policy) ResolvedInputPolicy {
	t.Helper()
	manifest := testCapabilityManifest(t, policy)
	pdfFormat, found := mistral.CandidateFormatByID("pdf")
	require.True(t, found)
	authorization, err := policy.Authorize(manifest, pdfFormat.ID)
	require.NoError(t, err)
	return ResolvedInputPolicy{Routes: map[string]InputRoute{
		pdfFormat.MediaType: {Format: pdfFormat, Authorization: authorization},
	}}
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
