package beeper

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

var errAttachmentSourceIO = errors.New("attachment source I/O failure")

// attachmentSourceFaults mark bytes that are missing or corrupt rather than temporarily unreadable.
var attachmentSourceFaults = []error{
	export.ErrInvalidContentHash, os.ErrNotExist,
	packstore.ErrPhysicalMissing, packstore.ErrPhysicalCorrupt, packstore.ErrContentMismatch,
	pack.ErrCorrupt, pack.ErrBlobMismatch, pack.ErrBadMagic, pack.ErrTruncated, pack.ErrChecksum,
}

type AttachmentMirror struct {
	store *store.Store
	blobs interface {
		OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
	}
	client              *docbankmedia.Client
	destination         string
	options             AttachmentMirrorOptions
	gate                func(context.Context) (func(), bool)
	uploadTimeout       time.Duration
	documentFingerprint string
}

// AttachmentMirrorOptions is the part of the Docbank config the mirror needs.
// The config package imports this one through recordingref, so it cannot be named here.
type AttachmentMirrorOptions struct {
	URL             string
	Folder          string
	AttachmentsDir  string
	DocumentProfile string
}

func NewAttachmentMirror(
	st *store.Store, blobs *attachmentstore.Store, client *docbankmedia.Client,
	destination string, options AttachmentMirrorOptions,
) *AttachmentMirror {
	return &AttachmentMirror{store: st, blobs: blobs, client: client, destination: destination, options: options}
}

func (w *AttachmentMirror) WithOperationGate(gate func(context.Context) (func(), bool)) *AttachmentMirror {
	w.gate = gate
	return w
}

// RunBatch runs passes for about 50 seconds and reports whether work remains.
func (w *AttachmentMirror) RunBatch(ctx context.Context) (bool, error) {
	return w.runBatch(ctx, time.Now().Add(50*time.Second))
}

func (w *AttachmentMirror) runBatch(ctx context.Context, deadline time.Time) (bool, error) {
	for {
		more, err := w.runPass(ctx, deadline)
		if !errors.Is(err, store.ErrDocbankAttachmentCursorChanged) {
			return more, err
		}
		if !time.Now().Before(deadline) {
			return true, nil
		}
	}
}

// runPass gives discovery and journal replay at most half of the budget, then uploads queued files.
// Discovery only queues intents, so journal reconciliation never waits for uploads.
func (w *AttachmentMirror) runPass(ctx context.Context, deadline time.Time) (bool, error) {
	queueDeadline := time.Now().Add(time.Until(deadline) / 2)
	discoverMore, err := w.discover(ctx, queueDeadline)
	if err != nil {
		return false, endMediaPass(err)
	}
	replayMore, err := w.replayAttachmentChanges(ctx, queueDeadline)
	if err != nil {
		return false, endMediaPass(err)
	}
	processMore, processErr := w.processDocuments(ctx, queueDeadline)
	uploadMore, err := w.drain(ctx, deadline)
	return discoverMore || replayMore || processMore || uploadMore, errors.Join(processErr, err)
}

// gated runs one Store step under the operation gate so backup freeze sees no writer.
func (w *AttachmentMirror) gated(ctx context.Context, step func() error) error {
	return gatedMediaStep(ctx, w.gate, step)
}

// discover queues files page by page until the scan finishes or the deadline passes.
func (w *AttachmentMirror) discover(ctx context.Context, deadline time.Time) (bool, error) {
	for {
		var page store.DocbankAttachmentPage
		err := w.gated(ctx, func() (err error) {
			page, err = w.store.NextDocbankAttachmentPage(ctx, w.destination)
			return err
		})
		if err != nil {
			return false, err
		}
		for i := range page.Files {
			page.Files[i] = attachmentUploadMetadata(page.Files[i])
		}
		err = w.gated(ctx, func() error {
			if err := w.store.QueueDocbankAttachmentIntents(ctx, w.destination, page.Files...); err != nil {
				return err
			}
			return w.store.FinishDocbankAttachmentPage(ctx, w.destination, page)
		})
		if err != nil {
			return false, err
		}
		if !page.More {
			return false, nil
		}
		if !time.Now().Before(deadline) {
			return true, nil
		}
	}
}

// drain uploads due intents until none remain or the deadline passes.
func (w *AttachmentMirror) drain(ctx context.Context, deadline time.Time) (bool, error) {
	packed, err := attachmentstore.New(store.NewDocbankAttachmentPackResolver(w.store), w.options.AttachmentsDir)
	if err != nil {
		return false, attachmentSourceError(err)
	}
	u := &attachmentUploader{mirror: w, packed: packed, handled: make(map[string]bool)}
	more, err := u.drain(ctx, deadline)
	return more, errors.Join(endMediaPass(err), u.reported, u.streak, attachmentSourceError(packed.Close()))
}

// attachmentUploader holds one pass of uploads and its destination failure streak.
type attachmentUploader struct {
	attachmentRetryStreak

	mirror  *AttachmentMirror
	packed  *attachmentstore.Store
	parent  int64
	handled map[string]bool
}

// attachmentRetryStreak keeps failures visible and stops after two consecutive retries.
type attachmentRetryStreak struct {
	streak   error
	reported error
}

func (u *attachmentUploader) drain(ctx context.Context, deadline time.Time) (bool, error) {
	w := u.mirror
	for {
		var intents []store.DocbankAttachmentDelivery
		err := w.gated(ctx, func() (err error) {
			intents, err = w.store.SendingDocbankAttachments(ctx, w.destination)
			return err
		})
		if err != nil {
			return false, err
		}
		intents = slices.DeleteFunc(intents, func(d store.DocbankAttachmentDelivery) bool {
			return u.handled[d.ContentHash]
		})
		if len(intents) == 0 {
			return false, nil
		}
		for _, d := range intents {
			if !time.Now().Before(deadline) {
				return true, nil
			}
			if err := u.send(ctx, d); err != nil {
				return false, err
			}
		}
	}
}

// send claims one intent, uploads it and records the outcome.
func (u *attachmentUploader) send(ctx context.Context, d store.DocbankAttachmentDelivery) error {
	w := u.mirror
	u.handled[d.ContentHash] = true
	if u.parent == 0 {
		parent, err := w.resolve(ctx)
		if err != nil {
			return err
		}
		u.parent = parent
	}
	err := w.gated(ctx, func() (err error) {
		d, err = w.store.SaveDocbankAttachmentIntent(ctx, w.destination, d)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ref, uploadErr := w.upload(ctx, u.parent, d, u.packed)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	reason := attachmentOutcome(uploadErr)
	if uploadErr != nil && reason == "" {
		return u.retryLater(ctx, d, uploadErr)
	}
	u.endStreak()
	ref.Endpoint, ref.Folder = strings.TrimRight(strings.TrimSpace(w.options.URL), "/"), w.options.Folder
	return w.gated(ctx, func() error {
		return w.store.RecordDocbankAttachment(ctx, w.destination, d, ref, reason)
	})
}

// retryLater delays a temporary failure and ends the pass on a second destination failure in a row.
func (u *attachmentUploader) retryLater(ctx context.Context, d store.DocbankAttachmentDelivery, uploadErr error) error {
	w := u.mirror
	err := w.gated(ctx, func() error {
		return w.store.RecordDocbankAttachmentRetry(ctx, w.destination, d, docbankmedia.ErrorCode(uploadErr))
	})
	if errors.Is(err, errBeeperMediaGateBusy) {
		u.reported = errors.Join(u.reported, uploadErr)
		return err
	}
	if !isDestinationFault(uploadErr) {
		u.endStreak()
		return err
	}
	return errors.Join(err, u.retry(errors.Join(uploadErr, err)))
}

func (u *attachmentRetryStreak) retry(err error) error {
	if u.streak != nil {
		joined := errors.Join(u.streak, err)
		u.streak = nil
		return joined
	}
	u.streak = err
	return nil
}

func (u *attachmentRetryStreak) endStreak() {
	u.reported = errors.Join(u.reported, u.streak)
	u.streak = nil
}

// isDestinationFault separates Docbank outages from per-file source and name conflicts.
func isDestinationFault(err error) bool {
	if errors.Is(err, errAttachmentSourceIO) {
		return false
	}
	httpErr, ok := errors.AsType[*docbankmedia.HTTPError](err)
	return !ok || httpErr.Status != http.StatusConflict
}

// attachmentOutcome identifies terminal file failures.
func attachmentOutcome(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errBeeperMediaSourceUnavailable) {
		return "source_unavailable"
	}
	if httpErr, ok := errors.AsType[*docbankmedia.HTTPError](err); ok {
		switch httpErr.Status {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge,
			http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
			return httpErr.Code
		default:
			return ""
		}
	}
	if errors.Is(err, docbankmedia.ErrInvalidRequest) || errors.Is(err, docbankmedia.ErrContentTooLarge) ||
		errors.Is(err, docbankmedia.ErrInvalidReceipt) {
		return docbankmedia.ErrorCode(err)
	}
	return ""
}

func attachmentUploadMetadata(d store.DocbankAttachmentDelivery) store.DocbankAttachmentDelivery {
	mediaType, _, err := mime.ParseMediaType(d.MIMEType)
	if err != nil {
		mediaType = "application/octet-stream"
	}
	name := strings.ToValidUTF8(path.Base(strings.ReplaceAll(d.Name, "\\", "/")), "")
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" {
		name = "attachment"
	}

	name = d.ContentHash[:12] + "-" + name
	d.Name, d.MIMEType = name, mediaType
	return d
}

// resolve finds the destination folder once per pass.
func (w *AttachmentMirror) resolve(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, beeperMediaActionTimeout)
	defer cancel()
	return w.client.EnsureFolder(ctx, w.options.Folder)
}

// sourceReader remembers a read failure so it is not mistaken for a Docbank fault.
type sourceReader struct {
	io.Reader

	err          error
	digest       hash.Hash
	expectedHash string
	remaining    int64
}

func (r *sourceReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if r.digest != nil {
		_, _ = r.digest.Write(p[:n])
		r.remaining -= int64(n)
		finished := r.remaining == 0 || errors.Is(err, io.EOF)
		if finished && hex.EncodeToString(r.digest.Sum(nil)) != r.expectedHash {
			err = packstore.ErrContentMismatch
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

// upload sends the archive copy, falling back to other recorded loose copies when its bytes are bad.
func (w *AttachmentMirror) upload(
	ctx context.Context, parent int64, d store.DocbankAttachmentDelivery, packed *attachmentstore.Store,
) (store.DocbankAttachmentRef, error) {
	reader, size, err := w.blobs.OpenStream(ctx, d.ContentHash)
	if errors.Is(err, os.ErrNotExist) && ctx.Err() == nil {
		reader, size, err = packed.OpenStream(ctx, d.ContentHash)
	}
	var ref store.DocbankAttachmentRef
	if err == nil {
		ref, err = w.uploadSource(ctx, parent, d, reader, size, false)
	} else {
		err = attachmentSourceError(err)
	}
	if ctx.Err() != nil {
		return ref, ctx.Err()
	}
	if attachmentOutcome(err) != "source_unavailable" {
		return ref, err
	}
	entry, lookupErr := w.store.GetAttachmentPackEntry(d.ContentHash)
	if lookupErr != nil {
		return ref, attachmentSourceError(lookupErr)
	}
	var excluded []string
	if entry == nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, packstore.ErrPhysicalMissing) {
		canonical, pathErr := export.StoragePath(w.options.AttachmentsDir, d.ContentHash)
		if pathErr != nil {
			return ref, attachmentSourceError(pathErr)
		}
		excluded = append(excluded, canonical)
	}
	return w.uploadRecordedCopies(ctx, parent, d, excluded)
}

// uploadRecordedCopies tries each verified loose copy the archive records for the hash.
func (w *AttachmentMirror) uploadRecordedCopies(
	ctx context.Context, parent int64, d store.DocbankAttachmentDelivery, excluded []string,
) (store.DocbankAttachmentRef, error) {
	paths, err := w.store.DocbankAttachmentStoragePaths(ctx, d.ContentHash)
	if err != nil {
		return store.DocbankAttachmentRef{}, attachmentSourceError(err)
	}
	recorded := func() ([]string, error) { return paths, nil }
	for {
		reader, size, _, selected, openErr := attachmentstore.OpenLooseCandidates(
			w.options.AttachmentsDir, d.ContentHash, recorded, excluded...)
		if openErr != nil {
			if ctx.Err() != nil {
				return store.DocbankAttachmentRef{}, ctx.Err()
			}
			return store.DocbankAttachmentRef{}, attachmentSourceError(openErr)
		}
		excluded = append(excluded, selected)
		// Each candidate needs a fresh request because verification may fail after bytes were sent.
		ref, err := w.uploadSource(ctx, parent, d, reader, size, true)
		if ctx.Err() != nil {
			return ref, ctx.Err()
		}
		if attachmentOutcome(err) != "source_unavailable" {
			return ref, err
		}
	}
}

func (w *AttachmentMirror) uploadSource(
	ctx context.Context, parent int64, d store.DocbankAttachmentDelivery,
	reader io.ReadCloser, size int64, loose bool,
) (ref store.DocbankAttachmentRef, err error) {
	defer func() {
		closeErr := reader.Close()
		// Early rejection can leave verification unfinished while the source is healthy.
		if err != nil && errors.Is(closeErr, pack.ErrVerificationIncomplete) {
			closeErr = nil
		}
		err = errors.Join(err, attachmentSourceError(closeErr))
	}()
	source := &sourceReader{Reader: reader}
	if loose {
		source.digest, source.expectedHash, source.remaining = sha256.New(), d.ContentHash, size
	}
	timeout := w.uploadTimeout
	if timeout == 0 {
		timeout = beeperMediaOperationTimeout(beeperMediaActionTimeout, size, beeperMediaUploadRate)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	receipt, err := w.client.UploadAttachment(ctx, parent, d.Name, d.MIMEType, d.ContentHash, size, source)
	if source.err != nil {
		return ref, attachmentSourceError(source.err)
	}
	if err != nil {
		return ref, err
	}
	return store.DocbankAttachmentRef{
		NodeID: receipt.Node.ID, VersionID: receipt.Node.VersionID, BlobHash: receipt.Node.BlobHash,
	}, nil
}

func attachmentSourceError(err error) error {
	if err == nil {
		return nil
	}
	for _, fault := range attachmentSourceFaults {
		if errors.Is(err, fault) {
			return errors.Join(errBeeperMediaSourceUnavailable, err)
		}
	}
	return errors.Join(errAttachmentSourceIO, err)
}

// replayAttachmentChanges queues files from journal changes once reconciliation is complete.
func (w *AttachmentMirror) replayAttachmentChanges(ctx context.Context, deadline time.Time) (bool, error) {
	consumer := store.DocbankAttachmentConsumerKey(w.destination)
	state, err := w.store.GetAttachmentChangeConsumer(ctx, consumer)
	if err != nil || !state.ReconciliationComplete {
		return false, err
	}
	if !time.Now().Before(deadline) {
		return true, nil
	}
	const batch = 500
	changes, err := w.store.ListAttachmentChanges(ctx, consumer, batch)
	if err != nil || len(changes) == 0 {
		return false, err
	}
	var processed int64
	more := len(changes) == batch
	for _, change := range changes {
		if !time.Now().Before(deadline) {
			more = true
			break
		}
		if err := w.replayChange(ctx, change); err != nil {
			return false, err
		}
		processed = change.Sequence
	}
	if processed != 0 {
		err := w.gated(ctx, func() error {
			return w.store.AdvanceAttachmentChangeConsumer(ctx, consumer, processed)
		})
		if err != nil {
			return false, err
		}
	}
	return more, nil
}

// replayChange queues the attachment a change made eligible, if any.
func (w *AttachmentMirror) replayChange(ctx context.Context, change store.AttachmentChange) error {
	switch change.EventKind {
	case "attachment_insert", "attachment_update", "message_live_enter":
	default:
		return nil
	}
	if change.NewAttachmentID == nil {
		return nil
	}
	return w.gated(ctx, func() error {
		d, err := w.store.DocbankAttachmentCandidate(ctx, *change.NewAttachmentID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return w.store.QueueDocbankAttachmentIntents(ctx, w.destination, attachmentUploadMetadata(d))
	})
}

// processDocuments visits due files until every request has an outcome or a next look.
func (w *AttachmentMirror) processDocuments(ctx context.Context, deadline time.Time) (bool, error) {
	if w.options.DocumentProfile == "" {
		return false, nil
	}
	actionCtx, cancel := context.WithTimeout(ctx, beeperMediaActionTimeout)
	profile, err := w.client.Profile(actionCtx, w.options.DocumentProfile)
	cancel()
	if err == nil && !profile.Rendition {
		err = errors.New("profile has no rendition stage")
	}
	if err != nil {
		if processingAnswer(docbankmedia.JobStatus{}, err).blocked {
			return false, w.processingBlocked(err)
		}
		return false, fmt.Errorf("document_profile %q: %w", w.options.DocumentProfile, err)
	}
	if profile.Fingerprint != w.documentFingerprint {
		if err := w.gated(ctx, func() error {
			return w.store.SaveDocbankAttachmentProcessingFingerprint(ctx, w.destination, w.options.DocumentProfile, profile.Fingerprint)
		}); err != nil {
			return false, endMediaPass(err)
		}
		w.documentFingerprint = profile.Fingerprint
	}
	var failures attachmentRetryStreak
	var cursor store.DocbankAttachmentProcessingCursor
	for {
		var files []store.DocbankAttachmentProcessing
		err := w.gated(ctx, func() (err error) {
			files, err = w.store.DocbankAttachmentsToProcess(ctx, w.destination, profile.Fingerprint, &cursor, 20)
			return err
		})
		if err != nil {
			return false, errors.Join(failures.reported, failures.streak, endMediaPass(err))
		}
		if len(files) == 0 {
			return false, errors.Join(failures.reported, failures.streak)
		}
		for _, d := range files {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if !time.Now().Before(deadline) {
				return true, errors.Join(failures.reported, failures.streak)
			}
			job, jobID, err := w.askProcessing(ctx, d)
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			answer := processingAnswer(job, err)
			if answer.blocked && answer.outcome == "" {
				return false, errors.Join(failures.reported, failures.streak, w.processingBlocked(err))
			}
			if !answer.retry {
				d.Outcome = answer.outcome
			}
			d.Fingerprint, d.JobID = profile.Fingerprint, jobID
			var next time.Time
			if answer.delay != 0 {
				next = time.Now().UTC().Add(answer.delay)
			}
			if saveErr := w.gated(ctx, func() error {
				return w.store.RecordDocbankAttachmentProcessing(ctx, w.destination, d, next)
			}); saveErr != nil {
				return false, errors.Join(failures.reported, failures.streak, err, endMediaPass(saveErr))
			}
			if answer.blocked {
				return false, errors.Join(failures.reported, failures.streak, w.processingBlocked(errors.New(answer.outcome)))
			}
			if answer.retry && isDestinationFault(err) {
				if stopErr := failures.retry(err); stopErr != nil {
					return false, errors.Join(failures.reported, stopErr)
				}
			} else {
				failures.endStreak()
			}
		}
	}
}

// askProcessing polls a saved job, or plans and starts one.
func (w *AttachmentMirror) askProcessing(ctx context.Context, d store.DocbankAttachmentProcessing) (docbankmedia.JobStatus, string, error) {
	ctx, cancel := context.WithTimeout(ctx, beeperMediaActionTimeout)
	defer cancel()
	if d.Outcome == "requested" || d.Outcome == "operator_required" {
		job, err := w.client.JobStatus(ctx, d.JobID)
		if !docbankmedia.IsNotFound(err) {
			return job, d.JobID, err
		}
	}
	selector := docbankmedia.ProcessingSelector{NodeID: d.NodeID, ContentVersionID: d.VersionID, Profile: w.options.DocumentProfile}
	plan, err := w.client.PlanProcessing(ctx, selector)
	if err != nil || plan.RenditionIneligibleReason != "" {
		return docbankmedia.JobStatus{State: "failed", FailureCode: plan.RenditionIneligibleReason}, "", err
	}
	jobID, err := w.client.StartProcessing(ctx, selector, plan.Fingerprint)
	return docbankmedia.JobStatus{State: "queued"}, jobID, err
}

func (w *AttachmentMirror) processingBlocked(err error) error {
	if httpErr, ok := errors.AsType[*docbankmedia.HTTPError](err); ok && httpErr.Reason != "" {
		err = fmt.Errorf("%s: %w", httpErr.Reason, err)
	}
	return fmt.Errorf("document_profile %q: check the profile and grant consent in Docbank: %w; after restoring consent or authority, run msgvault docbank attachments backfill", w.options.DocumentProfile, err)
}

type documentProcessingAnswer struct {
	outcome string
	delay   time.Duration
	retry   bool
	blocked bool
}

// processingAnswer is the single classification of Docbank answers into saved steps.
func processingAnswer(job docbankmedia.JobStatus, err error) documentProcessingAnswer {
	if err == nil {
		outcome := processingJobOutcome(job)
		answer := documentProcessingAnswer{outcome: outcome}
		switch outcome {
		case "requested":
			answer.delay = store.BeeperMediaPollDelay
		case "operator_required":
			answer.delay = store.BeeperMediaRetryDelay
		case "consent":
			answer.blocked = true
		}
		return answer
	}
	httpErr, isHTTP := errors.AsType[*docbankmedia.HTTPError](err)
	if errors.Is(err, docbankmedia.ErrCredentialUnavailable) || isHTTP && (httpErr.Status == http.StatusUnauthorized || httpErr.Status == http.StatusForbidden ||
		strings.HasPrefix(httpErr.Reason, "processing_consent_") || strings.HasPrefix(httpErr.Reason, "processing_profile_")) {
		return documentProcessingAnswer{blocked: true}
	}
	if errors.Is(err, docbankmedia.ErrInvalidReceipt) {
		return documentProcessingAnswer{outcome: docbankmedia.ErrorCode(err)}
	}
	if docbankmedia.Retryable(err) || isHTTP && httpErr.Status == http.StatusConflict && httpErr.Reason == "processing_plan_changed" {
		return documentProcessingAnswer{delay: store.BeeperMediaRetryDelay, retry: true}
	}
	code := docbankmedia.ErrorCode(err)
	if isHTTP && httpErr.Reason != "" {
		code = httpErr.Reason
	}
	return documentProcessingAnswer{outcome: code}
}

func processingJobOutcome(job docbankmedia.JobStatus) string {
	switch {
	case job.State == "completed" || job.State == "partial" || job.Phase == "embedding":
		// Docbank publishes rendition text before starting embeddings, even if embeddings fail.
		return "extracted"
	case job.State == "queued" || job.State == "running" || job.State == "retry_wait":
		return "requested"
	case job.State == "operator_required":
		return "operator_required"
	case job.State == "failed" || job.State == "abandoned":
		if job.FailureCode != "" && len(job.FailureCode) <= 128 && strings.Trim(job.FailureCode, "abcdefghijklmnopqrstuvwxyz_") == "" {
			return job.FailureCode
		}
		return job.State
	default:
		return "requested"
	}
}
