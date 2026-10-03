package documentindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/msgvault/internal/store"
)

// Docbank's canonical UTC timestamps retain all nine fractional digits.
// time.RFC3339Nano trims trailing zeros and can fail its boundary validator.
const doclingRenditionTimestampForm = "2006-01-02T15:04:05.000000000Z"

type DoclingWorkerConfig struct {
	Documents      DocumentsConfig
	ProfileID      string
	RebuildID      string
	LeaseOwner     string
	LeaseDuration  time.Duration
	RetryDelay     time.Duration
	ReplaceCurrent bool
}

type DoclingWorker struct {
	catalog       DocumentExtractionCatalog
	opener        DocumentAttachmentOpener
	provider      document.RenditionProvider
	config        DoclingWorkerConfig
	descriptor    document.RenditionDescriptor
	input         ResolvedInputPolicy
	normalization document.NormalizePolicy
}

type doclingEnvironmentSecrets struct{}

type doclingRequestAccountingKey struct{}

type doclingRequestAccounting struct {
	requests atomic.Int64
	retries  atomic.Int64

	mu                 sync.Mutex
	retryableResponses map[string]bool
}

// Count a retry only when the same route is called again after a transient
// response or transport error. Repeated status polls after a successful
// response are normal async job progress, not retries.
func (a *doclingRequestAccounting) before(request *http.Request) {
	a.requests.Add(1)
	key := request.Method + "\x00" + request.URL.EscapedPath()
	a.mu.Lock()
	if a.retryableResponses[key] {
		a.retries.Add(1)
		delete(a.retryableResponses, key)
	}
	a.mu.Unlock()
}

func (a *doclingRequestAccounting) after(request *http.Request, response *http.Response, err error) {
	retryable := err != nil || response != nil && doclingRetryableHTTPStatus(response.StatusCode)
	key := request.Method + "\x00" + request.URL.EscapedPath()
	a.mu.Lock()
	if retryable {
		if a.retryableResponses == nil {
			a.retryableResponses = make(map[string]bool)
		}
		a.retryableResponses[key] = true
	} else {
		delete(a.retryableResponses, key)
	}
	a.mu.Unlock()
}

func doclingRetryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError && status < 600
}

type doclingCountingTransport struct {
	base http.RoundTripper
}

func (t doclingCountingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	accounting, ok := request.Context().Value(doclingRequestAccountingKey{}).(*doclingRequestAccounting)
	if ok {
		accounting.before(request)
	}
	response, err := t.base.RoundTrip(request)
	if ok {
		accounting.after(request, response, err)
	}
	return response, err
}

func (doclingEnvironmentSecrets) ResolveSecret(_ context.Context, binding string) (string, error) {
	value := os.Getenv(binding)
	if value == "" {
		return "", errors.New("configured Docling credential is unavailable")
	}
	return value, nil
}

// NewDoclingClient sends only to the configured origin. Ambient proxy settings
// must not route private attachment bytes through a second recipient.
func NewDoclingClient(c *DocumentsConfig) (document.RenditionProvider, error) {
	if c == nil || c.Provider != ProviderDocling {
		return nil, errors.New("docling client requires provider=docling")
	}
	descriptor, err := c.DoclingDescriptor()
	if err != nil {
		return nil, err
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || defaultTransport == nil {
		return nil, errors.New("docling client requires an HTTP transport for proxy isolation")
	}
	transport := defaultTransport.Clone()
	transport.Proxy = nil
	var secrets docling.SecretResolver
	if c.APIKeyEnv != "" {
		secrets = doclingEnvironmentSecrets{}
	}
	client, err := docling.New(docling.Profile{
		Origin: c.Endpoint, Descriptor: descriptor, SecretBinding: c.APIKeyEnv,
		RequestTimeout: c.RequestTimeout, TotalTimeout: c.TotalTimeout,
		PollInterval: c.PollInterval, MaxPollAttempts: c.MaxPollAttempts,
		MaxResponseBytes: c.MaxResponseBytes, MaxDocumentBytes: c.MaxFileBytes,
	}, secrets, &http.Client{
		Transport:     doclingCountingTransport{base: transport},
		CheckRedirect: providerhttp.RefuseRedirects,
	})
	if err != nil {
		return nil, fmt.Errorf("configure Docling client: %w", err)
	}
	return client, nil
}

func NewDoclingWorker(catalog DocumentExtractionCatalog, opener DocumentAttachmentOpener,
	provider document.RenditionProvider, config DoclingWorkerConfig) (*DoclingWorker, error) {
	if catalog == nil || opener == nil || provider == nil {
		return nil, errors.New("docling worker requires catalog, attachment opener and provider")
	}
	if config.ProfileID == "" || config.LeaseOwner == "" || config.LeaseDuration <= 0 || config.LeaseDuration > time.Hour ||
		config.RetryDelay <= 0 || config.RetryDelay > 7*24*time.Hour || config.ReplaceCurrent != (config.RebuildID != "") {
		return nil, errors.New("docling worker configuration is incomplete")
	}
	if config.Documents.Provider != ProviderDocling {
		return nil, errors.New("docling worker requires provider=docling")
	}
	descriptor, err := config.Documents.DoclingDescriptor()
	if err != nil {
		return nil, err
	}
	if provider.Descriptor().Fingerprint != descriptor.Fingerprint {
		return nil, errors.New("docling provider does not match configured policy")
	}
	input, err := ResolveInputPolicy(&config.Documents, mistral.CapabilityManifest{})
	if err != nil {
		return nil, err
	}
	normalization, err := document.NewNormalizePolicy(config.Documents.MaxNormalizedChars)
	if err != nil {
		return nil, fmt.Errorf("configure Docling normalization: %w", err)
	}
	config.Documents.Scope.MessageTypes = slices.Clone(config.Documents.Scope.MessageTypes)
	return &DoclingWorker{catalog: catalog, opener: opener, provider: provider, config: config,
		descriptor: descriptor, input: input, normalization: normalization}, nil
}

func (w *DoclingWorker) ProcessCandidate(ctx context.Context, candidate store.DocumentExtractionCandidate) (result DocumentExtractionResult, runErr error) {
	result.CanonicalBlobHash = candidate.CanonicalBlobHash
	defer func() {
		if runErr != nil {
			_, result.FailureReasonCode = classifyDoclingFailure(runErr)
			result.FailureDetail = documentFailureDetail(runErr)
		}
	}()
	route, allowed := w.input.Routes[candidate.MIMEType]
	if !allowed {
		return result, errors.New("document media type is outside Docling scope")
	}
	scope := w.config.Documents.Scope.MessageTypes
	if len(scope) > 0 && !slices.Contains(scope, candidate.MessageType) {
		return result, errors.New("document message type is outside configured scope")
	}
	id, err := newDocumentExtractionID()
	if err != nil {
		return result, err
	}
	claim, err := w.catalog.ClaimDocumentExtraction(ctx, store.DocumentExtractionClaimInput{
		ExtractionID: id, ProfileID: w.config.ProfileID, RebuildID: w.config.RebuildID,
		CanonicalBlobHash: candidate.CanonicalBlobHash, ExtractionInputKey: originalDocumentInputKey,
		OccurrenceAttachmentID: candidate.AttachmentID, OccurrenceMIMEType: candidate.MIMEType,
		OccurrenceMessageType: candidate.MessageType, LeaseOwner: w.config.LeaseOwner,
		LeaseUntil: time.Now().UTC().Add(w.config.LeaseDuration), LocalBytes: candidate.Size,
		SourceSequence: candidate.SourceSequence, RequireNoHead: !w.config.ReplaceCurrent,
	})
	if err != nil {
		return result, err
	}
	workCtx, cancelWork, cancelRenewal, renewalDone, renewalErr := keepDocumentClaimAlive(ctx, w.catalog, claim, w.config.LeaseDuration)
	defer func() { cancelWork(); cancelRenewal(); <-renewalDone }()
	var requests doclingRequestAccounting
	workCtx = context.WithValue(workCtx, doclingRequestAccountingKey{}, &requests)
	var accounting documentPublicationAccounting
	fail := func(cause error) error {
		// Failed renders have no receipt. Count transport attempts rather
		// than inventing a request when validation or credentials fail.
		accounting.Requests = max(accounting.Requests, int(requests.requests.Load()))
		accounting.Retries = max(accounting.Retries, int(requests.retries.Load()))
		if renewErr := readRenewalError(renewalErr); renewErr != nil {
			cause = errors.Join(cause, renewErr)
		}
		terminal, reason := classifyDoclingFailure(cause)
		failure := store.DocumentExtractionFailure{Claim: claim, ReasonCode: reason, Detail: documentFailureDetail(cause), Terminal: terminal,
			RequestCount: accounting.Requests, RetryCount: accounting.Retries, ProviderLatencyMS: requestLatencyMillis(accounting.Latency)}
		if !terminal {
			failure.RetryAt = time.Now().UTC().Add(w.config.RetryDelay)
		}
		failureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), documentFailureCleanupTimeout)
		defer cancel()
		return errors.Join(cause, w.catalog.FailDocumentExtraction(failureCtx, failure))
	}
	content, err := w.readSource(workCtx, candidate, route.Format)
	if err != nil {
		return result, fail(fmt.Errorf("%w: %w", errDocumentPreparation, err))
	}
	upload, authorization, err := w.authorizeUpload(content, candidate, route.Format, claim)
	if err != nil {
		return result, fail(fmt.Errorf("%w: %w", errDocumentPreparation, err))
	}
	if err := workCtx.Err(); err != nil {
		return result, fail(err)
	}
	started := time.Now()
	rendered, err := document.RenderRendition(workCtx, w.provider, upload, authorization)
	accounting.Latency = time.Since(started)
	if err != nil {
		return result, fail(err)
	}
	accounting.ReturnedModel = w.descriptor.ID
	accounting.Requests = int(rendered.Receipt.Usage.Requests)
	accounting.Retries = int(rendered.Receipt.Usage.Retries)
	// The receipt counts returned evidence before any whole-document fallback.
	accounting.UnitsProcessed = max(len(rendered.Evidence.Units), int(rendered.Receipt.Usage.Units))
	if accounting.UnitsProcessed > w.config.Documents.MaxPagesPerDocument {
		return result, fail(errors.New("docling output exceeds the configured unit bound"))
	}
	if slices.Contains(rendered.Receipt.Warnings, "partial_success") {
		return result, fail(errors.New("docling returned a partial result"))
	}
	source, err := doclingSourceDocument(rendered)
	if err != nil {
		return result, fail(err)
	}
	normalized, err := document.NormalizeDocument(source, w.normalization)
	if err != nil {
		return result, fail(err)
	}
	publication, err := publicationFromDocument(claim, accounting, normalized)
	if err != nil {
		return result, fail(err)
	}
	publication.SourceBytes = claim.LocalBytes
	cancelRenewal()
	<-renewalDone
	if err := readRenewalError(renewalErr); err != nil {
		return result, fail(err)
	}
	renewCtx, cancelRenew := context.WithTimeout(workCtx, documentFailureCleanupTimeout)
	err = w.catalog.RenewDocumentExtractionClaim(renewCtx, claim, time.Now().UTC().Add(w.config.LeaseDuration))
	cancelRenew()
	if err != nil {
		return result, fail(fmt.Errorf("%w: %w", errDocumentLeaseRenewal, err))
	}
	if err := w.catalog.PublishDocumentExtraction(workCtx, publication); err != nil {
		return result, fail(fmt.Errorf("%w: %w", errDocumentPublication, err))
	}
	result.ExtractionID, result.Units, result.Chunks, result.Truncated = id, len(normalized.Units), len(normalized.Chunks), normalized.Truncated
	result.ProviderUnits = accounting.UnitsProcessed
	return result, nil
}

func (w *DoclingWorker) readSource(ctx context.Context, candidate store.DocumentExtractionCandidate, format mistral.CandidateFormat) ([]byte, error) {
	if candidate.Size <= 0 || candidate.Size > w.config.Documents.MaxFileBytes {
		return nil, errDocumentSizeBounds
	}
	source, size, err := w.opener.OpenStream(ctx, candidate.CanonicalBlobHash)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDocumentAttachmentOpen, err)
	}
	owned := &closeOnceReadCloser{ReadCloser: source}
	defer func() { _ = owned.Close() }()
	stopClose := context.AfterFunc(ctx, func() { _ = owned.Close() })
	defer stopClose()
	if size != candidate.Size {
		return nil, errDocumentSizeMismatch
	}
	content, readErr := io.ReadAll(io.LimitReader(owned, candidate.Size+1))
	closeErr := owned.Close()
	if err := errors.Join(ctx.Err(), readErr, closeErr); err != nil {
		return nil, err
	}
	if int64(len(content)) != candidate.Size {
		return nil, errDocumentSizeMismatch
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != candidate.CanonicalBlobHash {
		return nil, errors.New("document digest no longer matches reconciled metadata")
	}
	if format.ID == "html" {
		// Format detection has no HTML route; Docling parses the markup itself.
		if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			return nil, errors.New("invalid UTF-8 HTML source")
		}
		return content, nil
	}
	detected, err := mistral.DetectFormat(bytes.NewReader(content), size, candidate.MIMEType)
	if err != nil {
		return nil, fmt.Errorf("detect Docling source format: %w", err)
	}
	if detected.ID != format.ID || detected.MediaType != format.MediaType {
		return nil, errors.New("document format does not match configured scope")
	}
	return content, nil
}

type doclingMemoryUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
}

func (u *doclingMemoryUpload) Close() error                                { return nil }
func (u *doclingMemoryUpload) Metadata() document.AuthorizedUploadMetadata { return u.metadata }

func documentIdentityChecksum(value any) (string, error) {
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (w *DoclingWorker) authorizeUpload(content []byte, candidate store.DocumentExtractionCandidate,
	format mistral.CandidateFormat, claim store.DocumentExtractionClaim) (*doclingMemoryUpload, document.RenditionAuthorization, error) {
	return w.authorizeUploadAt(content, candidate, format, claim, time.Now().UTC())
}

func (w *DoclingWorker) authorizeUploadAt(content []byte, candidate store.DocumentExtractionCandidate,
	format mistral.CandidateFormat, claim store.DocumentExtractionClaim, now time.Time) (*doclingMemoryUpload, document.RenditionAuthorization, error) {
	capabilityChecksum, err := documentIdentityChecksum(struct {
		Descriptor string                  `json:"descriptor"`
		Format     mistral.CandidateFormat `json:"validated_format"`
		Validator  string                  `json:"validator"`
	}{w.descriptor.Fingerprint, format, "docbank-formatdetect+html/v1"})
	if err != nil {
		return nil, document.RenditionAuthorization{}, err
	}
	metadata := document.AuthorizedUploadMetadata{MediaFamily: format.Family, MediaType: format.MediaType,
		ByteLength: int64(len(content)), SHA256: candidate.CanonicalBlobHash,
		CapabilityRecordChecksum: capabilityChecksum, InputKind: document.RenditionInputOriginalFile}
	metadata.ProviderMetadataChecksum, err = documentIdentityChecksum(metadata)
	if err != nil {
		return nil, document.RenditionAuthorization{}, err
	}
	requestChecksum, err := documentIdentityChecksum(struct {
		Metadata   document.AuthorizedUploadMetadata `json:"upload"`
		Descriptor string                            `json:"descriptor"`
		Extraction string                            `json:"extraction"`
	}{metadata, w.descriptor.Fingerprint, claim.ExtractionID})
	if err != nil {
		return nil, document.RenditionAuthorization{}, err
	}
	bound := int(w.config.Documents.MaxResponseBytes)
	authorization := document.RenditionAuthorization{
		ProviderID: w.descriptor.ID, DescriptorFingerprint: w.descriptor.Fingerprint, PolicyFingerprint: w.descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: requestChecksum, SourceSHA256: metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily: metadata.MediaFamily, MediaType: metadata.MediaType, InputKind: metadata.InputKind,
		AllowedArtifactRoles: slices.Clone(w.descriptor.ArtifactRoles), MaxProviderMarkdownBytes: min(bound, 64<<20),
		MaxArtifactBytes: min(bound, 256<<20), MaxArtifacts: 1, MaxTotalResultBytes: bound,
		AuthorizedAt: now.UTC().Format(doclingRenditionTimestampForm),
		// Outlast the adapter's own total timeout, which starts after upload
		// preparation, so a slow job ends as a retryable timeout, not an expiry.
		ExpiresAt: now.UTC().Add(w.config.Documents.TotalTimeout + w.config.Documents.RequestTimeout).Format(doclingRenditionTimestampForm),
	}
	return &doclingMemoryUpload{Reader: bytes.NewReader(content), metadata: metadata}, authorization, nil
}

func doclingSourceDocument(result document.RenditionResult) (document.SourceDocument, error) {
	evidence := result.Evidence
	if evidence.Completeness != document.EvidenceComplete || evidence.UnitKind != document.EvidenceUnitPage || evidence.Family != "pdf" {
		if strings.TrimSpace(string(result.ProviderMarkdown)) == "" {
			return document.SourceDocument{}, errors.New("docling output has no complete searchable text")
		}
		return document.SourceDocument{Family: evidence.Family, UnitKind: "document",
			Units: []document.SourceUnit{{Index: 0, Markdown: string(result.ProviderMarkdown)}}}, nil
	}
	source := document.SourceDocument{Family: evidence.Family, UnitKind: "page"}
	for _, unit := range evidence.Units {
		source.Units = append(source.Units, document.SourceUnit{Index: int(unit.Locator.Start) - 1, Markdown: unit.Text})
	}
	return source, nil
}

func classifyDoclingFailure(err error) (bool, string) {
	if providerError, ok := errors.AsType[*document.RenditionProviderError](err); ok {
		switch providerError.Code() {
		// Resubmitting an interrupted job costs only time on the operator's server.
		case document.RenditionErrorUnknownJob, document.RenditionErrorCanceled, document.RenditionErrorAmbiguousSubmission:
			return false, "provider_interrupted"
		case document.RenditionErrorCapacity, document.RenditionErrorRateLimited, document.RenditionErrorTransient:
			return false, "provider_transient"
		case document.RenditionErrorAuthentication:
			return true, "provider_rejected"
		default:
			return true, "invalid_provider_output"
		}
	}
	return classifyDocumentExtractionFailure(err)
}
