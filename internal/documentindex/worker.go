package documentindex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/documentindex/mistral"
	"go.kenn.io/msgvault/internal/store"
)

const originalDocumentInputKey = "original"

const documentFailureCleanupTimeout = 5 * time.Second

var (
	errDocumentPreparation  = errors.New("document extraction preparation failed")
	errDocumentLeaseRenewal = errors.New("document extraction lease renewal failed")
	errDocumentPublication  = errors.New("document extraction publication failed")
)

type DocumentExtractionCatalog interface {
	ClaimDocumentExtraction(ctx context.Context, input store.DocumentExtractionClaimInput) (store.DocumentExtractionClaim, error)
	RenewDocumentExtractionClaim(ctx context.Context, claim store.DocumentExtractionClaim, leaseUntil time.Time) error
	PublishDocumentExtraction(ctx context.Context, publication store.DocumentExtractionPublication) error
	FailDocumentExtraction(ctx context.Context, failure store.DocumentExtractionFailure) error
}

type DocumentAttachmentOpener interface {
	OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

type MistralWorkerConfig struct {
	ProfileID        string
	RebuildID        string
	LeaseOwner       string
	LeaseDuration    time.Duration
	RetryDelay       time.Duration
	SpoolDirectory   string
	MaxFileBytes     int64
	MaxSpoolBytes    int64
	MinFreeBytes     int64
	MaxPages         int
	MessageTypes     []string
	ReplaceCurrent   bool
	NormalizePolicy  NormalizePolicy
	CapabilityPolicy mistral.CapabilityManifest
}

type MistralWorker struct {
	catalog      DocumentExtractionCatalog
	opener       DocumentAttachmentOpener
	processor    mistral.Processor
	config       MistralWorkerConfig
	formats      map[string]mistral.CandidateFormat
	messageTypes map[string]struct{}
}

type DocumentExtractionResult struct {
	ExtractionID      string
	CanonicalBlobHash string
	Units             int
	Chunks            int
	Truncated         bool
}

// NewMistralWorker binds runtime upload authority to the exact complete probe
// manifest. A documented format is not eligible unless that manifest recorded
// a passing authenticated probe for the pinned processor target.
func NewMistralWorker(
	catalog DocumentExtractionCatalog,
	opener DocumentAttachmentOpener,
	processor mistral.Processor,
	config MistralWorkerConfig,
) (*MistralWorker, error) {
	if catalog == nil || opener == nil || processor == nil {
		return nil, errors.New("mistral document worker requires catalog, attachment opener, and processor")
	}
	if config.ProfileID == "" || config.LeaseOwner == "" || config.SpoolDirectory == "" ||
		config.MaxFileBytes <= 0 || config.MaxPages <= 0 || config.LeaseDuration <= 0 ||
		config.MaxSpoolBytes < config.MaxFileBytes || config.MinFreeBytes <= 0 ||
		config.LeaseDuration > time.Hour || config.RetryDelay <= 0 || config.RetryDelay > 7*24*time.Hour {
		return nil, errors.New("mistral document worker configuration is incomplete")
	}
	if config.ReplaceCurrent != (config.RebuildID != "") {
		return nil, errors.New("mistral document worker replacement requires an exact rebuild")
	}
	if err := config.CapabilityPolicy.ValidateComplete(); err != nil {
		return nil, err
	}
	target := processor.Target()
	if target.Endpoint != config.CapabilityPolicy.Endpoint || target.Region != config.CapabilityPolicy.Region ||
		target.Model != config.CapabilityPolicy.RequestedModel {
		return nil, errors.New("mistral document worker target does not match capability manifest")
	}
	if config.MaxPages > config.CapabilityPolicy.MaxPages {
		return nil, errors.New("mistral document worker page limit exceeds probed policy")
	}
	if err := validateNormalizePolicy(config.NormalizePolicy); err != nil {
		return nil, err
	}
	formats := make(map[string]mistral.CandidateFormat)
	for _, result := range config.CapabilityPolicy.Results {
		if result.Status != mistral.ProbeStatusPassed {
			continue
		}
		format, ok := mistral.CandidateFormatByID(result.FormatID)
		if !ok {
			return nil, fmt.Errorf("mistral capability manifest contains unknown format %q", result.FormatID)
		}
		formats[format.MediaType] = format
	}
	messageTypes := make(map[string]struct{}, len(config.MessageTypes))
	for _, messageType := range config.MessageTypes {
		if messageType == "" {
			return nil, errors.New("mistral document worker message scope contains an empty type")
		}
		messageTypes[messageType] = struct{}{}
	}
	config.MessageTypes = slices.Clone(config.MessageTypes)
	return &MistralWorker{
		catalog: catalog, opener: opener, processor: processor, config: config,
		formats: formats, messageTypes: messageTypes,
	}, nil
}

// ProcessCandidate uploads one verified private spool, normalizes the returned
// Markdown entirely in memory, and atomically publishes only canonical local
// derivatives. The raw provider response and Markdown are never persisted.
func (w *MistralWorker) ProcessCandidate(
	ctx context.Context,
	candidate store.DocumentExtractionCandidate,
) (result DocumentExtractionResult, runErr error) {
	format, allowed := w.formats[candidate.MIMEType]
	if !allowed {
		return result, fmt.Errorf("document media type %q lacks passing capability authority", candidate.MIMEType)
	}
	if len(w.messageTypes) > 0 {
		if _, allowed = w.messageTypes[candidate.MessageType]; !allowed {
			return result, fmt.Errorf("document message type %q is outside configured scope", candidate.MessageType)
		}
	}
	if candidate.Size <= 0 || candidate.Size > w.config.MaxFileBytes {
		return result, errors.New("document candidate size is outside configured bounds")
	}

	source, authoritativeSize, err := w.opener.OpenStream(ctx, candidate.CanonicalBlobHash)
	if err != nil {
		return result, fmt.Errorf("open document attachment: %w", err)
	}
	if authoritativeSize != candidate.Size {
		_ = source.Close()
		return result, errors.New("document attachment size no longer matches reconciled metadata")
	}
	extractionID, err := newDocumentExtractionID()
	if err != nil {
		_ = source.Close()
		return result, err
	}
	claim, err := w.catalog.ClaimDocumentExtraction(ctx, store.DocumentExtractionClaimInput{
		ExtractionID: extractionID, ProfileID: w.config.ProfileID,
		RebuildID:         w.config.RebuildID,
		CanonicalBlobHash: candidate.CanonicalBlobHash, ExtractionInputKey: originalDocumentInputKey,
		OccurrenceAttachmentID: candidate.AttachmentID,
		OccurrenceMIMEType:     candidate.MIMEType,
		OccurrenceMessageType:  candidate.MessageType,
		LeaseOwner:             w.config.LeaseOwner, LeaseUntil: time.Now().UTC().Add(w.config.LeaseDuration),
		LocalBytes: authoritativeSize, SourceSequence: candidate.SourceSequence,
		RequireNoHead: !w.config.ReplaceCurrent,
	})
	if err != nil {
		_ = source.Close()
		return result, err
	}
	workCtx, cancelWork, renewalDone, renewalErr := w.keepClaimAlive(ctx, claim)
	defer func() {
		cancelWork()
		<-renewalDone
	}()
	document, cleanup, err := mistral.SpoolVerifiedSource(workCtx, source, mistral.SpoolOptions{
		Directory: w.config.SpoolDirectory, MediaType: candidate.MIMEType,
		ExpectedSize: authoritativeSize, ExpectedSHA256: candidate.CanonicalBlobHash,
		MaxBytes: w.config.MaxFileBytes, MaxSpoolBytes: w.config.MaxSpoolBytes,
		MinFreeBytes: w.config.MinFreeBytes,
	})
	if err != nil {
		err = fmt.Errorf("%w: %w", errDocumentPreparation, err)
		if renewErr := readRenewalError(renewalErr); renewErr != nil {
			err = errors.Join(err, renewErr)
		}
		err = errors.Join(err, w.recordFailureAfterError(ctx, claim, err, mistral.RequestMetrics{}))
		return result, err
	}
	defer func() { runErr = errors.Join(runErr, cleanup()) }()

	options := mistral.DefaultOptions()
	if format.Family == "pdf" {
		options.Pages = fmt.Sprintf("0-%d", w.config.MaxPages-1)
	}
	providerStarted := time.Now()
	providerResult, err := w.processor.Process(workCtx, document, options)
	providerMetrics := providerResult.Metrics
	if err != nil {
		providerMetrics = mistral.MetricsFromError(err)
		if renewErr := readRenewalError(renewalErr); renewErr != nil {
			err = errors.Join(err, renewErr)
		}
		err = errors.Join(err, w.recordFailureAfterError(ctx, claim, err, providerMetrics))
		return result, err
	}
	if renewErr := readRenewalError(renewalErr); renewErr != nil {
		err = errors.Join(renewErr, w.recordFailureAfterError(ctx, claim, renewErr, providerMetrics))
		return result, err
	}
	if providerMetrics.Requests == 0 {
		providerMetrics.Requests = 1
	}
	if providerMetrics.Latency <= 0 {
		providerMetrics.Latency = time.Since(providerStarted)
	}
	providerResult.Metrics = providerMetrics
	if len(providerResult.Pages) > w.config.MaxPages {
		err = fmt.Errorf("document provider returned %d units, limit %d", len(providerResult.Pages), w.config.MaxPages)
		err = errors.Join(err, w.recordFailureAfterError(ctx, claim, err, providerMetrics))
		return result, err
	}
	sourceDocument := SourceDocument{
		Family: format.Family, UnitKind: format.UnitKind,
		Units: make([]SourceUnit, len(providerResult.Pages)),
	}
	for i, page := range providerResult.Pages {
		sourceDocument.Units[i] = SourceUnit{
			Index: i, Markdown: page.Markdown, Header: page.Header, Footer: page.Footer,
			Dimensions: UnitDimensions{
				DPI: page.Dimensions.DPI, Height: page.Dimensions.Height, Width: page.Dimensions.Width,
			},
		}
	}
	normalized, err := NormalizeDocument(sourceDocument, w.config.NormalizePolicy)
	if err != nil {
		err = errors.Join(err, w.recordFailureAfterError(ctx, claim, err, providerMetrics))
		return result, err
	}
	publication, err := publicationFromNormalized(claim, providerResult, normalized)
	if err != nil {
		err = errors.Join(err, w.recordFailureAfterError(ctx, claim, err, providerMetrics))
		return result, err
	}
	if err := w.catalog.PublishDocumentExtraction(workCtx, publication); err != nil {
		if renewErr := readRenewalError(renewalErr); renewErr != nil {
			err = errors.Join(err, renewErr)
		} else {
			err = fmt.Errorf("%w: %w", errDocumentPublication, err)
		}
		err = errors.Join(err, w.recordFailureAfterError(ctx, claim, err, providerMetrics))
		return result, err
	}
	return DocumentExtractionResult{
		ExtractionID: extractionID, CanonicalBlobHash: candidate.CanonicalBlobHash,
		Units: len(normalized.Units), Chunks: len(normalized.Chunks), Truncated: normalized.Truncated,
	}, nil
}

func (w *MistralWorker) keepClaimAlive(
	ctx context.Context,
	claim store.DocumentExtractionClaim,
) (context.Context, context.CancelFunc, <-chan struct{}, <-chan error) {
	workCtx, cancelWork := context.WithCancel(ctx)
	done := make(chan struct{})
	errCh := make(chan error, 1)
	interval := w.config.LeaseDuration / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				renewCtx, cancelRenew := context.WithTimeout(
					context.WithoutCancel(workCtx), documentFailureCleanupTimeout,
				)
				err := w.catalog.RenewDocumentExtractionClaim(
					renewCtx, claim, time.Now().UTC().Add(w.config.LeaseDuration),
				)
				cancelRenew()
				if err != nil {
					errCh <- fmt.Errorf("%w: %w", errDocumentLeaseRenewal, err)
					cancelWork()
					return
				}
			}
		}
	}()
	return workCtx, cancelWork, done, errCh
}

func readRenewalError(errCh <-chan error) error {
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (w *MistralWorker) recordFailureAfterError(
	ctx context.Context,
	claim store.DocumentExtractionClaim,
	cause error,
	metrics mistral.RequestMetrics,
) error {
	failureCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		failureCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), documentFailureCleanupTimeout)
	}
	defer cancel()
	return w.recordFailure(failureCtx, claim, cause, metrics)
}

func (w *MistralWorker) recordFailure(
	ctx context.Context,
	claim store.DocumentExtractionClaim,
	cause error,
	metrics mistral.RequestMetrics,
) error {
	terminal, reason := classifyDocumentExtractionFailure(cause)
	failure := store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: reason, Terminal: terminal,
		RequestCount: metrics.Requests, RetryCount: metrics.Retries,
		ProviderLatencyMS: requestLatencyMillis(metrics.Latency),
	}
	if !terminal {
		failure.RetryAt = time.Now().UTC().Add(w.config.RetryDelay)
	}
	return w.catalog.FailDocumentExtraction(ctx, failure)
}

func classifyDocumentExtractionFailure(err error) (bool, string) {
	switch {
	case errors.Is(err, mistral.ErrSpoolCapacity):
		return false, "spool_capacity_unavailable"
	case errors.Is(err, errDocumentLeaseRenewal):
		return false, "lease_renewal_failed"
	case errors.Is(err, errDocumentPublication):
		return false, "publication_failed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false, "provider_interrupted"
	case errors.Is(err, errDocumentPreparation):
		return true, "invalid_local_source"
	case errors.Is(err, mistral.ErrTransientResponse):
		return false, "provider_transient"
	case errors.Is(err, mistral.ErrPermanentResponse):
		return true, "provider_rejected"
	case errors.Is(err, mistral.ErrResponseTooLarge):
		return true, "response_too_large"
	default:
		return true, "invalid_provider_output"
	}
}

func publicationFromNormalized(
	claim store.DocumentExtractionClaim,
	providerResult mistral.Result,
	normalized NormalizedDocument,
) (store.DocumentExtractionPublication, error) {
	if providerResult.UsageInfo == nil || providerResult.UsageInfo.PagesProcessed <= 0 || len(normalized.Chunks) == 0 {
		return store.DocumentExtractionPublication{}, errors.New("document extraction produced no publishable evidence")
	}
	publication := store.DocumentExtractionPublication{
		ExtractionID: claim.ExtractionID, ProfileID: claim.ProfileID,
		CanonicalBlobHash: claim.CanonicalBlobHash, ExtractionInputKey: claim.ExtractionInputKey,
		OccurrenceAttachmentID: claim.OccurrenceAttachmentID,
		OccurrenceMIMEType:     claim.OccurrenceMIMEType,
		OccurrenceMessageType:  claim.OccurrenceMessageType,
		LeaseOwner:             claim.LeaseOwner, LeaseFence: claim.LeaseFence,
		ReturnedModel: providerResult.Model, ProviderBytes: providerResult.UsageInfo.DocSizeBytes,
		UnitsProcessed: providerResult.UsageInfo.PagesProcessed, ManifestChecksum: normalized.Checksum,
		RequestCount: providerResult.Metrics.Requests, RetryCount: providerResult.Metrics.Retries,
		ProviderLatencyMS: requestLatencyMillis(providerResult.Metrics.Latency),
		Units:             make([]store.DocumentPublishedUnit, len(normalized.Units)),
		Chunks:            make([]store.DocumentPublishedChunk, len(normalized.Chunks)),
	}
	for i, unit := range normalized.Units {
		publication.Units[i] = store.DocumentPublishedUnit{
			Index: unit.Index, Kind: unit.Kind, Text: unit.Text, Header: unit.Header, Footer: unit.Footer,
			Width: unit.Dimensions.Width, Height: unit.Dimensions.Height, DPI: unit.Dimensions.DPI,
			Checksum: unit.Checksum, CharCount: unit.CharCount, Truncated: unit.Truncated,
		}
	}
	for i, chunk := range normalized.Chunks {
		published := store.DocumentPublishedChunk{
			Key: chunk.Key, Ordinal: chunk.Ordinal, Text: chunk.Text, HeadingPath: chunk.HeadingPath,
			FirstUnitIndex: chunk.Spans[0].UnitIndex, LastUnitIndex: chunk.Spans[len(chunk.Spans)-1].UnitIndex,
			Checksum: chunk.Checksum, CharCount: chunk.CharCount, Truncated: chunk.Truncated,
			Spans: make([]store.DocumentPublishedSpan, len(chunk.Spans)),
		}
		for spanIndex, span := range chunk.Spans {
			published.Spans[spanIndex] = store.DocumentPublishedSpan{
				UnitIndex: span.UnitIndex, CharStart: span.CharStart, CharEnd: span.CharEnd,
			}
		}
		publication.Chunks[i] = published
	}
	return publication, nil
}

func requestLatencyMillis(latency time.Duration) int64 {
	if latency <= 0 {
		return 0
	}
	milliseconds := latency.Milliseconds()
	if milliseconds == 0 {
		return 1
	}
	return milliseconds
}

func newDocumentExtractionID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate document extraction ID: %w", err)
	}
	return "docex_" + hex.EncodeToString(bytes[:]), nil
}
