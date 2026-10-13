package kataevidence

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/docbankmedia"
)

// DocbankTimeout bounds one citation or transcript passage's Docbank calls.
const DocbankTimeout = 5 * time.Second

// transcriptCheckTimeout bounds one download of a transcript's producer
// metadata, which keeps running after a caller's DocbankTimeout ends.
const transcriptCheckTimeout = time.Minute

// DocbankBindingStore finds the retained Docbank delivery of an attachment.
type DocbankBindingStore interface {
	KataDocbankBinding(ctx context.Context, destination string, attachmentID int64) (DocbankBinding, error)
}

// DocbankClient reads rendition windows from the configured Docbank.
type DocbankClient interface {
	Status(ctx context.Context, sourceID string) (docbankmedia.Receipt, error)
	Transcript(ctx context.Context, sourceID, sourceVersionID, contentVersionID string) (docbankmedia.MediaTranscript, error)
	EvidenceIdentity(ctx context.Context, contentVersionID, contentSHA256, profile string) (docbankmedia.EvidenceWindowRequest, error)
	CurrentVersion(ctx context.Context, nodeID int64) (string, error)
	ReadEvidenceWindow(ctx context.Context, request docbankmedia.EvidenceWindowRequest) (docbankmedia.EvidenceWindow, error)
}

// DocbankReader cites transcripts of files msgvault delivered to the one
// Docbank it is configured with, never a URL taken from a reference.
type DocbankReader struct {
	store       DocbankBindingStore
	client      DocbankClient
	destination func(context.Context) (string, error)
	revision    func(context.Context, int64) (string, error)
	mu          sync.Mutex
	verified    map[transcriptKey]bool
	checks      map[transcriptKey]*transcriptCheck
}

// NewDocbankReader reads bindings recorded under the destination key that
// destination returns.
func NewDocbankReader(store DocbankBindingStore, client DocbankClient, destination func(context.Context) (string, error), revision func(context.Context, int64) (string, error)) *DocbankReader {
	return &DocbankReader{store: store, client: client, destination: destination, revision: revision}
}

func (r *DocbankReader) checkRevision(ctx context.Context, binding DocbankBinding) error {
	revision, err := r.revision(ctx, binding.Reference.AttachmentID)
	if err != nil {
		return err
	}
	if revision == "" || revision != binding.Revision {
		return ErrChanged
	}
	return nil
}

// A pinned build and source revision keep producer metadata immutable; cache only successful checks.
type transcriptKey struct {
	revision, source, sourceVersion, contentVersion, contentHash, vault, input, profile, build, attachment, rendition string
}

// transcriptCheck is one shared download of a transcript's producer metadata.
type transcriptCheck struct {
	done chan struct{}
	err  error
}

func (r *DocbankReader) checkTranscript(ctx context.Context, binding DocbankBinding, request docbankmedia.EvidenceWindowRequest) error {
	// Visibility can change without changing a transcript build.
	status, err := r.client.Status(ctx, binding.SourceID)
	if notFound(err) {
		return ErrUnavailable
	}
	if err != nil {
		return err
	}
	if status.VaultUID != binding.VaultUID || status.SourceID != binding.SourceID {
		return ErrUnavailable
	}
	if status.SourceVersionID != binding.SourceVersionID || status.ContentVersionID != binding.ContentVersionID {
		return ErrChanged
	}
	if binding.SuppliedInputID != "" {
		switch {
		case status.CoverageState == "stale" || status.SuppliedInputID == "":
			return ErrUnavailable
		case !docbankmedia.SuppliedTranscriptMatches(binding.SuppliedInputID, status.SuppliedInputID):
			return ErrChanged
		}
	}
	key := transcriptKey{binding.Revision, binding.SourceID, binding.SourceVersionID, binding.ContentVersionID, binding.ContentSHA256, binding.VaultUID, binding.SuppliedInputID, binding.Profile, request.BuildID, request.RenditionAttachmentID, request.RenditionSHA256}
	r.mu.Lock()
	if r.verified[key] {
		r.mu.Unlock()
		return nil
	}
	check := r.checks[key]
	if check == nil {
		check = &transcriptCheck{done: make(chan struct{})}
		if r.checks == nil {
			r.checks = make(map[transcriptKey]*transcriptCheck)
		}
		r.checks[key] = check
		// A long transcript can outlast the caller's DocbankTimeout. The download
		// finishes under its own bound and caches success, so a retry can cite it.
		go r.runTranscriptCheck(context.WithoutCancel(ctx), key, check, binding, request)
	}
	r.mu.Unlock()
	select {
	case <-check.done:
		return check.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *DocbankReader) runTranscriptCheck(ctx context.Context, key transcriptKey, check *transcriptCheck, binding DocbankBinding, request docbankmedia.EvidenceWindowRequest) {
	ctx, cancel := context.WithTimeout(ctx, transcriptCheckTimeout)
	defer cancel()
	check.err = r.verifyTranscript(ctx, binding, request)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.checks, key)
	if check.err == nil {
		if len(r.verified) >= 64 {
			clear(r.verified)
		}
		if r.verified == nil {
			r.verified = make(map[transcriptKey]bool)
		}
		r.verified[key] = true
	}
	close(check.done)
}

// verifyTranscript checks that the served build came from the retained delivery.
func (r *DocbankReader) verifyTranscript(ctx context.Context, binding DocbankBinding, request docbankmedia.EvidenceWindowRequest) error {
	transcript, err := r.client.Transcript(ctx, binding.SourceID, binding.SourceVersionID, binding.ContentVersionID)
	if notFound(err) {
		return ErrUnavailable
	}
	if err != nil {
		return err
	}
	evidence := transcript.Transcript
	if transcript.VaultUID != binding.VaultUID || evidence == nil {
		return ErrUnavailable
	}
	if evidence.BuildID == "" {
		return ErrUnsupported
	}
	switch evidence.Origin {
	case "supplied":
		if !docbankmedia.SuppliedTranscriptMatches(binding.SuppliedInputID, evidence.SuppliedInputID) {
			return ErrUnavailable
		}
	case "generated":
		if binding.Profile == "supplied-transcript" || evidence.SuppliedInputID != "" || binding.SuppliedInputID != "" {
			return ErrUnavailable
		}
	default:
		return ErrUnavailable
	}
	if evidence.BuildID != request.BuildID {
		return ErrChanged
	}
	return nil
}

func (r *DocbankReader) binding(ctx context.Context, attachmentID int64) (DocbankBinding, error) {
	destination, err := r.destination(ctx)
	if err != nil {
		return DocbankBinding{}, err
	}
	return r.store.KataDocbankBinding(ctx, destination, attachmentID)
}

// LoadKataEvidenceSource reads the selected window of the file's current
// transcript.
func (r *DocbankReader) LoadKataEvidenceSource(ctx context.Context, selector Selector) (SourceRecord, error) {
	// Prepare has already validated the selector.
	start, end, _ := selectorRange(selector)
	binding, err := r.binding(ctx, selector.AttachmentID)
	if err != nil {
		return SourceRecord{}, err
	}
	if binding.Reference.MessageID != selector.MessageID {
		return SourceRecord{}, ErrUnavailable
	}
	if err := r.checkRevision(ctx, binding); err != nil {
		return SourceRecord{Display: binding.Display}, err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, DocbankTimeout)
	defer cancel()
	request, err := r.client.EvidenceIdentity(ctx, binding.ContentVersionID, binding.ContentSHA256, binding.Profile)
	if noRendition(err) && request.NodeID > 0 {
		err = r.classify(ctx, request.NodeID, binding, nil, err)
	}
	if err != nil {
		return SourceRecord{Display: binding.Display}, docbankError(parent, err)
	}
	request.VaultUID, request.Offset, request.MaxChars = binding.VaultUID, start, end-start
	window, err := r.client.ReadEvidenceWindow(ctx, request)
	if notFound(err) {
		err = r.classify(ctx, request.NodeID, binding, &request, err)
	}
	if err != nil {
		return SourceRecord{Display: binding.Display}, docbankError(parent, err)
	}
	if err := r.checkTranscript(ctx, binding, request); err != nil {
		return SourceRecord{Display: binding.Display}, docbankError(parent, err)
	}
	if err := r.checkRevision(parent, binding); err != nil {
		return SourceRecord{Display: binding.Display}, err
	}
	ref := binding.Reference
	ref.DocbankRendition = &DocbankReference{VaultUID: request.VaultUID, NodeID: request.NodeID, ContentVersionID: request.ContentVersionID,
		ContentSHA256: request.ContentSHA256, RenditionAttachmentID: request.RenditionAttachmentID, BuildID: request.BuildID, RenditionSHA256: request.RenditionSHA256}
	return SourceRecord{Reference: ref, Text: window.Text, TextStart: window.ActualStart, More: !window.EOF, Display: binding.Display}, nil
}

// ReadKataEvidenceSource reads the cited window with up to ContextRunes on each
// side, for a context read, and only while the attachment's retained delivery
// still names the cited file.
func (r *DocbankReader) ReadKataEvidenceSource(ctx context.Context, ref Reference) (SourceRecord, error) {
	p := ref.DocbankRendition
	if ref.Kind != "docbank_rendition" || p == nil {
		return SourceRecord{}, ErrInvalidReference
	}
	binding, err := r.binding(ctx, ref.AttachmentID)
	if err != nil {
		return SourceRecord{}, err
	}
	a := binding.Reference
	if a.ArchiveUID != ref.ArchiveUID || a.MessageID != ref.MessageID || a.SourceType != ref.SourceType || a.SourceIdentifier != ref.SourceIdentifier ||
		a.SourceMessageID != ref.SourceMessageID || a.OccurrenceKey != ref.OccurrenceKey || binding.VaultUID != p.VaultUID {
		return SourceRecord{}, ErrUnavailable
	}
	if binding.ContentVersionID != p.ContentVersionID || !strings.EqualFold(binding.ContentSHA256, p.ContentSHA256) {
		return SourceRecord{Display: binding.Display}, ErrChanged
	}
	if err := r.checkRevision(ctx, binding); err != nil {
		return SourceRecord{Display: binding.Display}, err
	}
	from := max(0, p.StartRune-ContextRunes)
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, DocbankTimeout)
	defer cancel()
	request := docbankmedia.EvidenceWindowRequest{VaultUID: p.VaultUID, NodeID: p.NodeID, ContentVersionID: p.ContentVersionID,
		ContentSHA256: p.ContentSHA256, RenditionAttachmentID: p.RenditionAttachmentID, BuildID: p.BuildID, RenditionSHA256: p.RenditionSHA256,
		Offset: from, MaxChars: p.EndRune - from + ContextRunes}
	window, err := r.client.ReadEvidenceWindow(ctx, request)
	if notFound(err) {
		err = r.classify(ctx, p.NodeID, binding, &request, err)
	}
	if err != nil {
		return SourceRecord{Display: binding.Display}, docbankError(parent, err)
	}
	if err := r.checkTranscript(ctx, binding, request); err != nil {
		return SourceRecord{Display: binding.Display}, docbankError(parent, err)
	}
	if err := r.checkRevision(parent, binding); err != nil {
		return SourceRecord{Display: binding.Display}, err
	}
	return SourceRecord{Reference: ref, Text: window.Text, TextStart: window.ActualStart, Display: binding.Display}, nil
}

// classify decides what a Docbank 404 about the bound file means. Docbank
// answers 404 for a deleted or trashed node, a version that is no longer
// current, and a version with no selected rendition alike, so it asks the
// node. window is the window request that 404'd, or nil when the rendition
// select did.
func (r *DocbankReader) classify(ctx context.Context, nodeID int64, binding DocbankBinding, window *docbankmedia.EvidenceWindowRequest, failure error) error {
	httpErr, _ := errors.AsType[*docbankmedia.HTTPError](failure)
	current, err := r.client.CurrentVersion(ctx, nodeID)
	switch {
	case err != nil:
		return err
	case current != binding.ContentVersionID:
		return ErrChanged
	case window == nil:
		if httpErr != nil && httpErr.Reason == "evidence_unavailable" {
			return ErrUnavailable
		}
		return ErrUnprocessed
	}
	identity, err := r.client.EvidenceIdentity(ctx, binding.ContentVersionID, binding.ContentSHA256, binding.Profile)
	switch {
	case noRendition(err):
		return ErrUnprocessed
	case err != nil:
		return err
	case identity.RenditionAttachmentID != window.RenditionAttachmentID || identity.BuildID != window.BuildID || identity.RenditionSHA256 != window.RenditionSHA256:
		return ErrChanged
	}
	if httpErr != nil && httpErr.Reason == "evidence_unavailable" {
		return ErrUnavailable
	}
	// The file and its transcript are there, so this Docbank lacks the window route (before v0.15.0).
	return ErrUnsupported
}

func notFound(err error) bool {
	httpErr, ok := errors.AsType[*docbankmedia.HTTPError](err)
	return ok && httpErr.Status == http.StatusNotFound
}

// noRendition reports whether Docbank selected no rendition of a version.
func noRendition(err error) bool {
	return notFound(err) || errors.Is(err, docbankmedia.ErrNoRendition)
}

// docbankError keeps what classify decided and reports a failure as retryable
// only when a retry can change it. A missing transcript file or a window past
// the transcript's end is unavailable; a refused request or credential is a
// configuration problem, so unsupported; an unreachable or failing Docbank or a
// timeout is a retryable archive failure. The caller's own cancellation stays an error.
func docbankError(parent context.Context, err error) error {
	switch {
	case parent.Err() != nil, errors.Is(err, ErrChanged), errors.Is(err, ErrUnprocessed), errors.Is(err, ErrFailed), errors.Is(err, ErrUnavailable), errors.Is(err, ErrUnsupported):
		return err
	case errors.Is(err, docbankmedia.ErrEvidenceUnavailable):
		return ErrUnavailable
	case errors.Is(err, docbankmedia.ErrRenditionFailed):
		return ErrFailed
	case errors.Is(err, docbankmedia.ErrCredentialUnavailable), errors.Is(err, docbankmedia.ErrInvalidReceipt),
		errors.Is(err, docbankmedia.ErrProfileUnavailable):
		return ErrUnsupported
	}
	if httpErr, ok := errors.AsType[*docbankmedia.HTTPError](err); ok {
		switch {
		case httpErr.Reason == "rendition_failed":
			return ErrFailed
		case httpErr.Status == http.StatusConflict && httpErr.Reason == "rendition_selection_stale":
			return ErrChanged
		case httpErr.Status == http.StatusRequestedRangeNotSatisfiable, httpErr.Status == http.StatusServiceUnavailable && httpErr.Reason == "content_missing",
			httpErr.Status == http.StatusRequestEntityTooLarge && httpErr.Reason == "rendition_text_too_large":
			return ErrUnavailable
		case httpErr.Status == http.StatusBadRequest, httpErr.Status == http.StatusUnprocessableEntity, httpErr.Status == http.StatusUnauthorized, httpErr.Status == http.StatusForbidden:
			return ErrUnsupported
		}
	}
	// %v drops a timeout from the chain so it reads as a 503, not a request timeout.
	return fmt.Errorf("%w: %v", ErrDocbankUnavailable, err) //nolint:errorlint
}
