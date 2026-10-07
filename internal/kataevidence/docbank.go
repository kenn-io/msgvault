package kataevidence

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/docbankmedia"
)

// DocbankTimeout bounds one citation or transcript passage's Docbank calls.
const DocbankTimeout = 5 * time.Second

// DocbankBindingStore finds the retained Docbank delivery of an attachment.
type DocbankBindingStore interface {
	KataDocbankBinding(ctx context.Context, destination string, attachmentID int64) (DocbankBinding, error)
}

// DocbankClient reads rendition windows from the configured Docbank.
type DocbankClient interface {
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
}

// NewDocbankReader reads bindings recorded under the destination key that
// destination returns.
func NewDocbankReader(store DocbankBindingStore, client DocbankClient, destination func(context.Context) (string, error)) *DocbankReader {
	return &DocbankReader{store: store, client: client, destination: destination}
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
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, DocbankTimeout)
	defer cancel()
	request, err := r.client.EvidenceIdentity(ctx, binding.ContentVersionID, binding.ContentSHA256, binding.Profile)
	if notFound(err) && request.NodeID > 0 {
		err = r.classify(ctx, request.NodeID, binding, nil)
	}
	if err != nil {
		return SourceRecord{}, docbankError(parent, err)
	}
	request.VaultUID, request.Offset, request.MaxChars = binding.VaultUID, start, end-start
	window, err := r.client.ReadEvidenceWindow(ctx, request)
	if notFound(err) {
		err = r.classify(ctx, request.NodeID, binding, &request)
	}
	if err != nil {
		return SourceRecord{}, docbankError(parent, err)
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
		return SourceRecord{}, ErrChanged
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
		err = r.classify(ctx, p.NodeID, binding, &request)
	}
	if err != nil {
		return SourceRecord{}, docbankError(parent, err)
	}
	return SourceRecord{Reference: ref, Text: window.Text, TextStart: window.ActualStart, Display: binding.Display}, nil
}

// classify decides what a Docbank 404 about the bound file means. Docbank
// answers 404 for a deleted or trashed node, a version that is no longer
// current, and a version with no selected rendition alike, so it asks the
// node. window is the window request that 404'd, or nil when the rendition
// select did.
func (r *DocbankReader) classify(ctx context.Context, nodeID int64, binding DocbankBinding, window *docbankmedia.EvidenceWindowRequest) error {
	current, err := r.client.CurrentVersion(ctx, nodeID)
	switch {
	case err != nil:
		return err
	case current != binding.ContentVersionID:
		return ErrChanged
	case window == nil:
		return ErrUnprocessed
	}
	identity, err := r.client.EvidenceIdentity(ctx, binding.ContentVersionID, binding.ContentSHA256, binding.Profile)
	switch {
	case notFound(err):
		return ErrUnprocessed
	case err != nil:
		return err
	case identity.RenditionAttachmentID != window.RenditionAttachmentID || identity.BuildID != window.BuildID || identity.RenditionSHA256 != window.RenditionSHA256:
		return ErrChanged
	}
	// The file and its transcript are there, so this Docbank lacks the window route (before v0.15.0).
	return ErrUnsupported
}

func notFound(err error) bool {
	httpErr, ok := errors.AsType[*docbankmedia.HTTPError](err)
	return ok && httpErr.Status == http.StatusNotFound
}

// docbankError keeps what classify decided and reports a failure as retryable
// only when a retry can change it. A missing transcript file or a window past
// the transcript's end is unavailable; a refused request or credential is a
// configuration problem, so unsupported; an unreachable or failing Docbank or a
// timeout is a retryable archive failure. The caller's own cancellation stays an error.
func docbankError(parent context.Context, err error) error {
	switch {
	case parent.Err() != nil, errors.Is(err, ErrChanged), errors.Is(err, ErrUnprocessed), errors.Is(err, ErrUnavailable), errors.Is(err, ErrUnsupported):
		return err
	case errors.Is(err, docbankmedia.ErrEvidenceUnavailable):
		return ErrUnavailable
	case errors.Is(err, docbankmedia.ErrCredentialUnavailable):
		return ErrUnsupported
	}
	if httpErr, ok := errors.AsType[*docbankmedia.HTTPError](err); ok {
		switch {
		case httpErr.Status == http.StatusRequestedRangeNotSatisfiable, httpErr.Status == http.StatusServiceUnavailable && httpErr.Reason == "content_missing":
			return ErrUnavailable
		case httpErr.Status == http.StatusBadRequest, httpErr.Status == http.StatusUnprocessableEntity, httpErr.Status == http.StatusUnauthorized, httpErr.Status == http.StatusForbidden:
			return ErrUnsupported
		}
	}
	// %v drops a timeout from the chain so it reads as a 503, not a request timeout.
	return fmt.Errorf("%w: %v", ErrDocbankUnavailable, err) //nolint:errorlint
}
