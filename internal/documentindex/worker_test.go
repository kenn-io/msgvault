package documentindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	content := validWorkerPDF()
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
	require.NoError(t, err)
	require.NotNil(t, catalog.publication)
	assert.True(t, catalog.claimInput.RequireNoHead)
	assert.Equal(t, int64(11), catalog.claimInput.SourceSequence)
	assert.Equal(t, "# Invoice\nAmount 42", catalog.publication.Units[0].Text)
	assert.NotContains(t, catalog.publication.Units[0].Text, "private")
	assert.Equal(t, []string{"Invoice"}, catalog.publication.Chunks[0].HeadingPath)
	assert.Equal(t, 1, catalog.publication.RequestCount)
	assert.Zero(t, catalog.publication.RetryCount)
	assert.Positive(t, catalog.publication.ProviderLatencyMS)
	assert.Equal(t, 1, result.Units)
	assert.Equal(t, 1, result.Chunks)
	assert.Nil(t, catalog.failure)

	entries, err := os.ReadDir(worker.config.SpoolDirectory)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the package reservation lock remains after publication")
}

func TestMistralWorkerRecordsSanitizedRetryWithoutPublishing(t *testing.T) {
	content := validWorkerPDF()
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	processor := &workerProcessor{err: mistral.ErrTransientResponse}
	worker := newTestMistralWorker(t, catalog, &workerOpener{content: content}, processor)

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(t, err, mistral.ErrTransientResponse)
	require.NotNil(t, catalog.failure)
	assert.False(t, catalog.failure.Terminal)
	assert.Equal(t, "provider_transient", catalog.failure.ReasonCode)
	assert.True(t, catalog.failure.RetryAt.After(time.Now().UTC()))
	assert.Nil(t, catalog.publication)
}

func TestMistralWorkerReleasesClaimAfterRequestCancellation(t *testing.T) {
	content := validWorkerPDF()
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{}
	ctx, cancel := context.WithCancel(t.Context())
	processor := &workerProcessor{err: context.Canceled, cancel: cancel}
	worker := newTestMistralWorker(t, catalog, &workerOpener{content: content}, processor)

	_, err := worker.ProcessCandidate(ctx, store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: hex.EncodeToString(hash[:]), MIMEType: "application/pdf",
		Size: int64(len(content)), MessageType: "email", SourceSequence: 1,
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, catalog.failure)
	require.NoError(t, catalog.failureContextErr)
	assert.False(t, catalog.failure.Terminal)
	assert.Equal(t, "provider_interrupted", catalog.failure.ReasonCode)
}

func TestMistralWorkerReleasesClaimAfterPublicationFailure(t *testing.T) {
	content := validWorkerPDF()
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
	require.ErrorIs(t, err, errDocumentPublication)
	require.NotNil(t, catalog.failure)
	assert.False(t, catalog.failure.Terminal)
	assert.Equal(t, "publication_failed", catalog.failure.ReasonCode)
}

func TestMistralWorkerCancelsProcessingWhenLeaseRenewalFails(t *testing.T) {
	content := validWorkerPDF()
	hash := sha256.Sum256(content)
	catalog := &workerCatalog{renewErr: errors.New("synthetic renewal failure")}
	processor := &workerProcessor{block: make(chan struct{})}
	worker := newTestMistralWorker(t, catalog, &workerOpener{content: content}, processor)
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
	opener := &workerOpener{content: []byte("unused")}
	catalog := &workerCatalog{}
	worker := newTestMistralWorker(t, catalog, opener, &workerProcessor{})

	_, err := worker.ProcessCandidate(t.Context(), store.DocumentExtractionCandidate{
		AttachmentID: 1, CanonicalBlobHash: strings.Repeat("a", 64),
		MIMEType: "application/pdf", Size: (1 << 20) + 1, MessageType: "email",
	})
	require.ErrorContains(t, err, "candidate size is outside configured bounds")
	assert.Zero(t, opener.opened)
	require.NotNil(t, catalog.failure)
	assert.True(t, catalog.failure.Terminal)
	assert.Equal(t, "invalid_local_source", catalog.failure.ReasonCode)
}

func TestMistralWorkerClaimsBeforeWritingPrivateSpool(t *testing.T) {
	content := validWorkerPDF()
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
	content := validWorkerPDF()
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
	require.ErrorIs(t, err, mistral.ErrCapabilityContract)
	require.NotNil(t, catalog.failure)
	assert.True(t, catalog.failure.Terminal)
	assert.Equal(t, "provider_capability_changed", catalog.failure.ReasonCode)
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
	values := policy.Values()
	manifest := mistral.CapabilityManifest{
		SchemaVersion: mistral.CapabilitySchemaVersion, ProbeFixtureContract: 2,
		ObservedOn: time.Now().UTC().Format(time.DateOnly), Endpoint: values.Endpoint,
		Region: values.Region, RequestedModel: values.Model, MaxUnits: values.MaxUnits,
		Results: make([]mistral.CapabilityResult, 0, len(mistral.CandidateFormats())),
	}
	for _, candidate := range mistral.CandidateFormats() {
		result := mistral.CapabilityResult{
			FormatID: candidate.ID, Family: candidate.Family, MediaType: candidate.MediaType,
			UnitKind: candidate.UnitKind, Status: mistral.ProbeStatusPassed,
			FixtureDigest: strings.Repeat("0", 16), RequestFingerprint: strings.Repeat("0", 64),
			ReturnedModel: values.Model, UnitCount: 1, UnitsProcessed: 1,
			UnitBoundMethod: mistral.UnitBoundNone,
		}
		if candidate.ID == "pdf" {
			result.RequestFingerprint = testRequestFingerprint(t, values, candidate)
			result.UnitCount = 2
			result.UnitsProcessed = 2
			result.UnitBoundMethod = mistral.UnitBoundProviderRequest
			result.FixtureUnits = 2
			result.BoundRequestedUnits = 1
			result.BoundUnitsProcessed = 1
		}
		manifest.Results = append(manifest.Results, result)
	}
	require.NoError(t, manifest.ValidateComplete())
	return manifest
}

func testRequestFingerprint(
	t *testing.T,
	values mistral.PolicyValues,
	candidate mistral.CandidateFormat,
) string {
	t.Helper()
	payload := struct {
		Version   int                     `json:"version"`
		Endpoint  string                  `json:"endpoint"`
		Model     string                  `json:"model"`
		Candidate mistral.CandidateFormat `json:"candidate"`
		Options   struct {
			Pages         string `json:"pages"`
			ExtractHeader bool   `json:"extract_header"`
			ExtractFooter bool   `json:"extract_footer"`
		} `json:"options"`
	}{Version: 2, Endpoint: values.Endpoint, Model: values.Model, Candidate: candidate}
	payload.Options.Pages = fmt.Sprintf("0-%d", values.MaxUnits-1)
	payload.Options.ExtractHeader = values.ExtractHeader
	payload.Options.ExtractFooter = values.ExtractFooter
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
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

func validWorkerPDF() []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output,
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xref,
	)
	return output.Bytes()
}
