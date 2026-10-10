package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/emlx"
	"go.kenn.io/msgvault/internal/store"
)

// emlxChunkSize bounds how many occurrences share one batch of ledger reads.
const emlxChunkSize = 200

// These boundaries bind real I/O in production and let tests count actual
// parser/raw/ingestion calls while still executing those real functions.
type emlxImportIO struct {
	parse  func(string, int64) (*emlx.Message, error)
	raw    func(context.Context, *store.Store, int64) ([]byte, error)
	ingest rawMessageIngestFunc
}

func defaultEmlxImportIO(opts EmlxImportOptions) emlxImportIO {
	ingest := rawMessageIngestFunc(opts.IngestFunc)
	if ingest == nil {
		ingest = func(
			ctx context.Context, st *store.Store, sid int64, identifier, dest string, labels []int64,
			target, hash string, raw []byte, date time.Time, log *slog.Logger,
		) error {
			return ingestRawMessageWithCompletion(ctx, st, sid, identifier, dest, labels, target, hash,
				raw, date, log, opts.RemoteImages, "", true)
		}
	}
	raw := func(ctx context.Context, st *store.Store, id int64) ([]byte, error) {
		return st.GetMessageRawContext(ctx, id)
	}
	return emlxImportIO{parse: emlx.ParseFile, raw: raw, ingest: ingest}
}

type emlxOutcomeKind int

const (
	emlxOutcomeNone emlxOutcomeKind = iota
	// emlxOutcomeSkipped: the archive already held everything this file offers,
	// or the archived message was deleted.
	emlxOutcomeSkipped
	// emlxOutcomeUnchanged: a receipt matched, so the file was not read.
	emlxOutcomeUnchanged
	emlxOutcomeAdded
	emlxOutcomeUpdated
)

type emlxOutcome struct {
	kind              emlxOutcomeKind
	partial, restored int64
}

// emlxRetryableError marks per-file work that stays pending for a later run
// without failing the whole import: unreadable or oversized files, attachment
// cache problems, merge budgets, and attachment or search-index failures.
type emlxRetryableError struct{ err error }

func (e *emlxRetryableError) Error() string { return e.err.Error() }
func (e *emlxRetryableError) Unwrap() error { return e.err }

func emlxRetryable(err error) error {
	if err == nil {
		return nil
	}
	return &emlxRetryableError{err: err}
}

// isEmlxRetryable reports whether every error in err's tree is retryable, so
// a database failure joined with a retryable one still fails the run.
func isEmlxRetryable(err error) bool {
	//nolint:errorlint // Walks wrapped and joined errors explicitly.
	switch e := err.(type) {
	case *emlxRetryableError:
		return true
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		for _, child := range children {
			if !isEmlxRetryable(child) {
				return false
			}
		}
		return len(children) > 0
	case interface{ Unwrap() error }:
		return isEmlxRetryable(e.Unwrap())
	}
	return false
}

// emlxOccurrenceImporter imports one physical .emlx file at a time. Ledger
// reads are batched per chunk of a mailbox; every mutation uses fresh reads.
type emlxOccurrenceImporter struct {
	st                   *store.Store
	sourceID, syncID     int64
	root, prefix, policy string
	opts                 EmlxImportOptions
	io                   emlxImportIO
	log                  *slog.Logger
}

// emlxVisit is the metadata read for one file before its content is touched.
type emlxVisit struct {
	file, id, rel  string
	label          string
	pathErr        error
	item           *store.SourceImportItem
	reconcile      bool // the receipt was pending before this visit
	receipt        emlxReceipt
	decoded        bool
	signature      string
	eligible       bool
	fingerprintErr error
}

// emlxChunk carries one batch of prefetched occurrence and target state.
// Targets mutated while the chunk is processed are forgotten and reread.
type emlxChunk struct {
	labelID int64
	visits  []*emlxVisit
	targets map[string]store.EmlxTargetState
}

func (c *emlxChunk) forget(target string) { delete(c.targets, target) }

func (r *emlxOccurrenceImporter) prefetch(
	ctx context.Context, files []string, label string, labelID int64,
) (*emlxChunk, error) {
	chunk := &emlxChunk{labelID: labelID, visits: make([]*emlxVisit, len(files))}
	ids := make([]string, 0, len(files))
	for i, file := range files {
		v := &emlxVisit{file: file, label: label}
		chunk.visits[i] = v
		rel, err := filepath.Rel(r.root, file)
		if err != nil || !store.IsEmlxOccurrenceID(r.prefix+filepath.ToSlash(rel)) {
			v.pathErr = emlxRetryable(fmt.Errorf("unsupported EMLX path %q", file))
			continue
		}
		v.rel = filepath.ToSlash(rel)
		v.id = r.prefix + v.rel
		ids = append(ids, v.id)
		fingerprint, eligible, err := emlx.Fingerprint(ctx, file)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		v.signature = emlxSignature(fingerprint, label, r.opts.MaxMessageBytes)
		v.eligible = eligible && err == nil
		v.fingerprintErr = emlxRetryable(err)
	}
	items, err := r.st.EmlxOccurrencesContext(ctx, r.sourceID, ids)
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, v := range chunk.visits {
		item, ok := items[v.id]
		if !ok {
			continue
		}
		v.item = &item
		v.reconcile = item.Status == "pending"
		v.receipt, v.decoded = decodeEmlxReceipt(item.Checksum, v.id)
		if r.hitCandidate(v) && !slices.Contains(targets, v.receipt.Target) {
			targets = append(targets, v.receipt.Target)
		}
	}
	chunk.targets, err = r.st.EmlxTargetsContext(ctx, r.sourceID, labelID, targets)
	return chunk, err
}

// hitCandidate reports whether v's completed receipt still describes the file
// and its attachment dependencies, so its content need not be read.
func (r *emlxOccurrenceImporter) hitCandidate(v *emlxVisit) bool {
	return r.opts.RemoteImages == nil && !r.opts.FullReconcile && v.item != nil &&
		v.item.Status == "imported" && v.decoded && v.eligible && v.receipt.Signature == v.signature
}

func (r *emlxOccurrenceImporter) process(
	ctx context.Context, chunk *emlxChunk, i int,
) (out emlxOutcome, retErr error) {
	v := chunk.visits[i]
	if v.pathErr != nil {
		return out, v.pathErr
	}
	// Revoke an old receipt on any incomplete attempt. Healthy hits never rewrite
	// it; target completion evidence governs all receipts of the same target.
	defer func() {
		if retErr != nil && v.item != nil && v.item.Status == "imported" {
			retErr = errors.Join(retErr, r.markPending(ctx, v, v.item.Checksum))
		}
	}()
	if r.hitCandidate(v) {
		hit, done, err := r.tryHit(ctx, chunk, v)
		if err != nil || done {
			return hit, err
		}
	}
	return r.importCold(ctx, chunk, v)
}

func (r *emlxOccurrenceImporter) markPending(ctx context.Context, v *emlxVisit, checksum string) error {
	item := store.SourceImportItem{
		SourceID: r.sourceID, Provider: "emlx-occurrence", ProviderID: v.id, Name: v.rel,
		Checksum: checksum, Status: "pending",
	}
	if err := r.st.PutEmlxLedgerItemsContext(ctx, item); err != nil {
		return err
	}
	v.item = &item
	return nil
}

func (r *emlxOccurrenceImporter) target(
	ctx context.Context, chunk *emlxChunk, id string,
) (store.EmlxTargetState, error) {
	if state, ok := chunk.targets[id]; ok {
		return state, nil
	}
	states, err := r.st.EmlxTargetsContext(ctx, r.sourceID, chunk.labelID, []string{id})
	return states[id], err
}

// tryHit completes an occurrence from its receipt without reading the file.
// It reports done=false when the archived target needs the file's content.
func (r *emlxOccurrenceImporter) tryHit(
	ctx context.Context, chunk *emlxChunk, v *emlxVisit,
) (out emlxOutcome, done bool, err error) {
	targetID := v.receipt.Target
	target, err := r.target(ctx, chunk, targetID)
	if err != nil {
		return out, true, err
	}
	if target.Deleted {
		out.kind = emlxOutcomeSkipped
		return out, true, nil
	}
	if target.MessageID == 0 || !target.HasRaw {
		return out, false, nil
	}
	out.kind = emlxOutcomeUnchanged
	if !targetComplete(target, targetID, r.policy) {
		if err := r.recoverTarget(ctx, chunk, targetID, target); err != nil {
			return out, true, err
		}
		out.kind = emlxOutcomeUpdated
		return out, true, nil
	}
	// The receipt was written after this mailbox label was applied; restore it
	// only if it was removed since.
	if !target.HasLabel {
		if err := r.st.AddMessageLabels(target.MessageID, []int64{chunk.labelID}); err != nil {
			return out, true, err
		}
		chunk.forget(targetID)
	}
	return out, true, nil
}

// recoverTarget finishes unfinished attachment or search work from the newest
// committed raw, never from older source bytes. Ingestion applies this
// occurrence's label alongside the labels already on the message.
func (r *emlxOccurrenceImporter) recoverTarget(
	ctx context.Context, chunk *emlxChunk, targetID string, target store.EmlxTargetState,
) error {
	current, err := r.io.raw(ctx, r.st, target.MessageID)
	if err != nil {
		return err
	}
	if err := r.creditIntent(ctx, chunk, targetID, target, current); err != nil {
		return err
	}
	return r.completeTarget(ctx, chunk, targetID, target, current, target.InternalDate.Time)
}

// creditIntent settles an interrupted ingest before anything else changes the
// target. When the archived raw is the one the ingest was writing, the
// intended occurrence's receipt gains its attachment parts. Otherwise the
// write never landed and that occurrence retries its parts. A credited
// occurrence later in this chunk sees its new receipt, not the prefetched one.
func (r *emlxOccurrenceImporter) creditIntent(
	ctx context.Context, chunk *emlxChunk, targetID string, target store.EmlxTargetState,
	current []byte,
) error {
	intent := emlxPendingIntent(target, targetID)
	if intent == nil || intent.Raw != emlxRawDigest(current) {
		return nil
	}
	items, err := r.st.EmlxOccurrencesContext(ctx, r.sourceID, []string{intent.Occurrence})
	if err != nil {
		return err
	}
	receipt := emlxReceipt{Version: emlxReceiptVersion, ID: intent.Occurrence, Target: targetID}
	status := "pending"
	if item, ok := items[intent.Occurrence]; ok {
		if prior, ok := decodeEmlxReceipt(item.Checksum, intent.Occurrence); ok {
			// The file has since moved on to another message; its receipt for
			// that message is newer than this historical write.
			if prior.Target != targetID {
				return nil
			}
			receipt, status = prior, item.Status
		}
	}
	receipt.SourceParts = maps.Clone(receipt.SourceParts)
	if receipt.SourceParts == nil {
		receipt.SourceParts = make(map[string]string, len(intent.Parts))
	}
	maps.Copy(receipt.SourceParts, intent.Parts)
	checksum, err := encodeEmlxReceipt(receipt)
	if err != nil {
		return err
	}
	item := store.SourceImportItem{
		SourceID: r.sourceID, Provider: "emlx-occurrence", ProviderID: intent.Occurrence,
		Name: intent.Occurrence[65:], Checksum: checksum, Status: status,
	}
	if err := r.st.PutEmlxLedgerItemsContext(ctx, item); err != nil {
		return err
	}
	for _, v := range chunk.visits {
		if v.id == intent.Occurrence {
			v.item, v.receipt, v.decoded = &item, receipt, true
		}
	}
	return nil
}

// completeTarget ingests raw into the shared target and records its completion.
func (r *emlxOccurrenceImporter) completeTarget(
	ctx context.Context, chunk *emlxChunk, targetID string, target store.EmlxTargetState,
	raw []byte, date time.Time,
) error {
	completed, err := r.ingestTarget(ctx, chunk, targetID, target, raw, date, nil)
	if err != nil {
		return err
	}
	return r.st.PutEmlxLedgerItemsContext(ctx, completed)
}

// ingestTarget marks the shared target dirty and ingests raw, returning the
// completion record for the caller to publish. Ledger writes use the existing
// source-generation fence, so a failed dirty write aborts before any shared
// message/raw/blob/index mutation.
func (r *emlxOccurrenceImporter) ingestTarget(
	ctx context.Context, chunk *emlxChunk, targetID string, target store.EmlxTargetState,
	raw []byte, date time.Time, intent *emlxIntent,
) (store.SourceImportItem, error) {
	chunk.forget(targetID)
	labels := []int64{chunk.labelID}
	if target.MessageID != 0 {
		existing, err := r.st.MessageLabelIDsContext(ctx, target.MessageID)
		if err != nil {
			return store.SourceImportItem{}, err
		}
		for _, id := range existing {
			if !slices.Contains(labels, id) {
				labels = append(labels, id)
			}
		}
	}
	completion := emlxCompletion{Target: targetID, Policy: r.policy, Run: r.syncID, Intent: intent}
	dirty, err := emlxTargetItem(r.sourceID, completion, "pending")
	if err != nil {
		return dirty, err
	}
	if err := r.st.PutEmlxLedgerItemsContext(ctx, dirty); err != nil {
		return dirty, err
	}
	hash := strings.TrimPrefix(targetID, "emlx-")
	err = r.io.ingest(ctx, r.st, r.sourceID, r.opts.Identifier, r.opts.AttachmentsDir, labels, targetID,
		hash, raw, date, r.log)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return dirty, err
	}
	// Test overrides cannot authorize a receipt for a nonexistent raw target.
	states, err := r.st.EmlxTargetsContext(ctx, r.sourceID, chunk.labelID, []string{targetID})
	if err != nil {
		return dirty, err
	}
	if state := states[targetID]; state.MessageID == 0 || !state.HasRaw || state.Deleted {
		return dirty, errors.New("EMLX target has no live archived raw")
	}
	completion.Intent = nil
	return emlxTargetItem(r.sourceID, completion, "imported")
}

// emlxCandidate is one occurrence's content merged with the archived target.
type emlxCandidate struct {
	msg          *emlx.Message
	size         int64
	targetID     string
	target       store.EmlxTargetState
	current      []byte
	recovered    bool
	merged       emlx.MergeResult
	restoreErr   error
	fallbackDate time.Time
}

func (r *emlxOccurrenceImporter) importCold(
	ctx context.Context, chunk *emlxChunk, v *emlxVisit,
) (out emlxOutcome, err error) {
	// A stale clean path must become pending before attempting new content.
	if v.item != nil && v.item.Status == "imported" {
		if err := r.markPending(ctx, v, v.item.Checksum); err != nil {
			return out, err
		}
	}
	c, err := r.readCandidate(ctx, chunk, v)
	if c != nil && emlx.IsPartial(filepath.Base(v.file)) {
		out.partial = 1
	}
	if err != nil || c.target.Deleted {
		if err == nil {
			out.kind = emlxOutcomeSkipped
		}
		return out, err
	}
	if r.needsIngest(v, c) {
		intent := &emlxIntent{Occurrence: v.id, Parts: c.merged.SourceParts, Raw: emlxRawDigest(c.merged.Raw)}
		completed, err := r.ingestTarget(ctx, chunk, c.targetID, c.target, c.merged.Raw, c.fallbackDate, intent)
		if err != nil {
			return out, err
		}
		out.kind = emlxOutcomeAdded
		if c.target.MessageID != 0 {
			out.kind = emlxOutcomeUpdated
		}
		out.restored = int64(c.merged.ChangedParts)
		return out, r.publishReceipt(ctx, v, c, completed)
	}
	out.kind = emlxOutcomeSkipped
	if c.recovered {
		out.kind = emlxOutcomeUpdated
	} else if !c.target.HasLabel {
		chunk.forget(c.targetID)
		// Ingestion also applies the mailbox label, so it retries one that
		// failed to attach.
		if err := r.st.AddMessageLabels(c.target.MessageID, []int64{chunk.labelID}); err != nil {
			if err := r.completeTarget(ctx, chunk, c.targetID, c.target, c.current, c.fallbackDate); err != nil {
				return out, err
			}
			out.kind = emlxOutcomeUpdated
		}
	}
	return out, r.publishReceipt(ctx, v, c)
}

// readCandidate reads the file and merges its attachments with the archived
// target. Problems with the file itself are retryable.
func (r *emlxOccurrenceImporter) readCandidate(
	ctx context.Context, chunk *emlxChunk, v *emlxVisit,
) (*emlxCandidate, error) {
	info, err := os.Stat(v.file)
	if err != nil {
		return nil, emlxRetryable(err)
	}
	if info.Size() > r.opts.MaxMessageBytes {
		return nil, emlxRetryable(fmt.Errorf("file exceeds size limit %d", r.opts.MaxMessageBytes))
	}
	msg, err := r.io.parse(v.file, r.opts.MaxMessageBytes)
	if err != nil {
		return nil, emlxRetryable(err)
	}
	c := &emlxCandidate{msg: msg, size: info.Size(), targetID: "emlx-" + msg.SourceHash}
	c.target, err = r.target(ctx, chunk, c.targetID)
	if err != nil || c.target.Deleted {
		return c, err
	}
	if c.target.HasRaw {
		if c.current, err = r.io.raw(ctx, r.st, c.target.MessageID); err != nil {
			return c, err
		}
		if !targetComplete(c.target, c.targetID, r.policy) {
			if err := r.recoverTarget(ctx, chunk, c.targetID, c.target); err != nil {
				return c, err
			}
			c.recovered = true
		}
	}
	c.fallbackDate = msg.PlistDate
	if c.fallbackDate.IsZero() {
		c.fallbackDate = c.target.InternalDate.Time
	}
	if err := r.mergeCandidate(ctx, v, c); err != nil {
		return c, emlxRetryable(err)
	}
	return c, nil
}

func (r *emlxOccurrenceImporter) mergeCandidate(ctx context.Context, v *emlxVisit, c *emlxCandidate) error {
	msg := c.msg
	// Source-part acknowledgments are independent of completion settings,
	// filesystem cache eligibility and which parts fit the current budget.
	var acknowledged map[string]string
	if c.target.HasRaw && v.decoded && v.receipt.Target == c.targetID {
		acknowledged = v.receipt.SourceParts
	}
	if len(c.current) == 0 && len(msg.RestorationParts) == 0 {
		c.merged = emlx.MergeResult{Raw: msg.Raw, SourceParts: acknowledged}
		c.restoreErr = msg.RestorationError
		return nil
	}
	// An identical restored candidate already satisfies the source budget;
	// only differing archived parts need a separate combined-budget merge.
	if len(c.current) > 0 && len(msg.RestorationParts) > 0 && !bytes.Equal(msg.Raw, c.current) {
		// Preserve feasible progress even when another sibling could not be read.
		// The occurrence stays pending until all supported work succeeds.
		var err error
		c.merged, err = emlx.MergeAttachmentsFromFile(ctx, msg.OriginalRaw, c.current, v.file,
			r.opts.MaxMessageBytes, acknowledged)
		c.restoreErr = err
		return ctx.Err()
	}
	var err error
	c.merged, err = emlx.MergeAttachments(msg.OriginalRaw, msg.Raw, c.current, msg.RestorationParts,
		r.opts.MaxMessageBytes, acknowledged)
	c.restoreErr = msg.RestorationError
	return err
}

// needsIngest reports whether the merged candidate must be written. Forced
// reconciliation completes each shared target at most once per run.
func (r *emlxOccurrenceImporter) needsIngest(v *emlxVisit, c *emlxCandidate) bool {
	if !c.target.HasRaw || !bytes.Equal(c.current, c.merged.Raw) {
		return true
	}
	forced := r.opts.FullReconcile || v.reconcile
	if !forced || c.recovered {
		return false
	}
	completion, ok := emlxTargetCompletion(c.target, c.targetID, r.policy)
	return !ok || completion.Run != r.syncID
}

func (r *emlxOccurrenceImporter) receipt(v *emlxVisit, c *emlxCandidate) emlxReceipt {
	return emlxReceipt{
		Version: emlxReceiptVersion, ID: v.id, SourceParts: c.merged.SourceParts, Target: c.targetID,
	}
}

// publishReceipt completes the occurrence once all supported work succeeded
// and the file and its dependencies did not change while they were read. A
// target completed by this occurrence is published in the same transaction.
// When other work remains, the archive still holds the merged raw, so the
// pending receipt keeps the attachment parts this occurrence contributed.
func (r *emlxOccurrenceImporter) publishReceipt(
	ctx context.Context, v *emlxVisit, c *emlxCandidate, completed ...store.SourceImportItem,
) error {
	receipt, err := r.readyReceipt(ctx, v, c)
	if err == nil {
		return r.st.PutEmlxLedgerItemsContext(ctx, append(completed, receipt)...)
	}
	checksum, encodeErr := encodeEmlxReceipt(r.receipt(v, c))
	if encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	pending := store.SourceImportItem{
		SourceID: r.sourceID, Provider: "emlx-occurrence", ProviderID: v.id, Name: v.rel,
		Checksum: checksum, Status: "pending",
	}
	putErr := r.st.PutEmlxLedgerItemsContext(context.WithoutCancel(ctx), append(completed, pending)...)
	if putErr == nil {
		v.item = &pending
	}
	return errors.Join(err, putErr)
}

func (r *emlxOccurrenceImporter) readyReceipt(
	ctx context.Context, v *emlxVisit, c *emlxCandidate,
) (store.SourceImportItem, error) {
	var item store.SourceImportItem
	if v.fingerprintErr != nil || c.restoreErr != nil {
		return item, emlxRetryable(errors.Join(v.fingerprintErr, c.restoreErr))
	}
	if c.merged.Incomplete {
		return item, emlxRetryable(errors.New("attachment restoration exceeds current merge budget; " +
			"retry with a larger --max-message-bytes limit (bytes, including MIME encoding)"))
	}
	fingerprint, afterEligible, err := emlx.Fingerprint(ctx, v.file)
	if err != nil {
		return item, emlxRetryable(err)
	}
	after := emlxSignature(fingerprint, v.label, r.opts.MaxMessageBytes)
	if v.eligible && (!afterEligible || v.signature != after) {
		return item, emlxRetryable(errors.New("EMLX dependencies changed during import"))
	}
	receipt := r.receipt(v, c)
	// Cold occurrences retain content evidence but have no metadata signature
	// authorizing a future content-read shortcut.
	if r.opts.RemoteImages == nil && v.eligible && afterEligible {
		receipt.Signature = v.signature
	}
	checksum, err := encodeEmlxReceipt(receipt)
	return store.SourceImportItem{
		SourceID: r.sourceID, Provider: "emlx-occurrence", ProviderID: v.id, Name: v.rel,
		Checksum: checksum, Size: c.size, Status: "imported",
	}, err
}
