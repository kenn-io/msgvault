package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/emlx"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

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
		ingest = func(ctx context.Context, st *store.Store, sid int64, identifier, dest string, labels []int64, target, hash string, raw []byte, date time.Time, log *slog.Logger) error {
			return ingestRawMessageWithCompletion(ctx, st, sid, identifier, dest, labels, target, hash, raw, date, log, opts.RemoteImages, "", true)
		}
	}
	return emlxImportIO{parse: emlx.ParseFile, raw: func(ctx context.Context, st *store.Store, id int64) ([]byte, error) {
		return st.GetMessageRawContext(ctx, id)
	}, ingest: ingest}
}

type emlxOutcome struct {
	kind              string
	partial, restored int64
}

func completeEmlxTarget(ctx context.Context, st *store.Store, sourceID int64, target, policy string, raw []byte, labels []int64, date time.Time, opts EmlxImportOptions, io emlxImportIO, log *slog.Logger) error {
	// Both publications use the existing source-generation fence. A failed
	// dirty write aborts before any shared message/raw/blob/index mutation.
	if err := putEmlxTarget(ctx, st, sourceID, target, policy, "pending"); err != nil {
		return err
	}
	if err := io.ingest(ctx, st, sourceID, opts.Identifier, opts.AttachmentsDir, labels, target, strings.TrimPrefix(target, "emlx-"), raw, date, log); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Test overrides cannot authorize a receipt for a nonexistent raw target.
	states, err := st.EmlxTargetsContext(ctx, sourceID, []string{target})
	if err != nil {
		return err
	}
	state := states[target]
	if state.MessageID == 0 || !state.HasRaw || state.Deleted {
		return errors.New("EMLX target has no live archived raw")
	}
	return putEmlxTarget(ctx, st, sourceID, target, policy, "imported")
}

func emlxAllLabels(ctx context.Context, st *store.Store, messageID int64, labels []int64) ([]int64, error) {
	labels = append([]int64(nil), labels...)
	if messageID != 0 {
		old, err := st.MessageLabelIDsContext(ctx, messageID)
		if err != nil {
			return nil, err
		}
		for _, id := range old {
			if !slices.Contains(labels, id) {
				labels = append(labels, id)
			}
		}
	}
	return labels, nil
}

func processEmlxOccurrence(ctx context.Context, st *store.Store, sourceID int64, root, prefix, file, label string, labels []int64, policy string, opts EmlxImportOptions, io emlxImportIO, log *slog.Logger) (out emlxOutcome, retErr error) {
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return out, err
	}
	rel = filepath.ToSlash(rel)
	if !validEmlxRelative(rel) {
		return out, errors.New("invalid EMLX relative path")
	}
	id := prefix + rel
	item, err := st.EmlxOccurrenceContext(ctx, sourceID, id)
	if err != nil {
		return out, err
	}
	receipt, decoded := emlxReceipt{}, false
	if item != nil {
		receipt, decoded = decodeEmlxReceipt(item.Checksum, id)
	}
	// Pending occurrences must finish ingestion even when their source has not
	// changed. This also retains root reconciliation across interrupted runs.
	reconcile := item != nil && item.Status == "pending"
	signature, eligible, fingerprintErr := emlx.Fingerprint(ctx, file)
	if fingerprintErr != nil {
		eligible = false
	}
	signature = emlxDigest(fmt.Sprintf("%d:%s:%s:%d", emlxReceiptVersion, signature, label, opts.MaxMessageBytes))
	cacheAllowed := opts.RemoteImages == nil && opts.IngestFunc == nil
	// Revoke an old receipt on any incomplete attempt. Healthy hits never rewrite
	// it; target completion evidence governs all receipts of the same target.
	defer func() {
		if retErr != nil && item != nil && item.Status == "imported" {
			err := st.PutEmlxLedgerItemContext(ctx, store.SourceImportItem{SourceID: sourceID, Provider: "emlx-occurrence", ProviderID: id, Name: rel, Checksum: item.Checksum, Status: "pending"})
			retErr = errors.Join(retErr, err)
		}
	}()
	if cacheAllowed && !opts.FullReconcile && !reconcile && eligible && decoded && receipt.Signature == signature {
		states, err := st.EmlxTargetsContext(ctx, sourceID, []string{receipt.Target})
		if err != nil {
			return out, err
		}
		target := states[receipt.Target]
		if target.Deleted {
			out.kind = "skipped"
			return out, nil
		}
		recovered := false
		if target.MessageID != 0 && target.HasRaw && !targetComplete(target, receipt.Target, policy) {
			current, err := io.raw(ctx, st, target.MessageID)
			if err != nil {
				return out, err
			}
			all, err := emlxAllLabels(ctx, st, target.MessageID, labels)
			if err != nil {
				return out, err
			}
			if err := completeEmlxTarget(ctx, st, sourceID, receipt.Target, policy, current, all, target.InternalDate.Time, opts, io, log); err != nil {
				return out, err
			}
			states, err = st.EmlxTargetsContext(ctx, sourceID, []string{receipt.Target})
			if err != nil {
				return out, err
			}
			target = states[receipt.Target]
			recovered = true
		}
		if targetComplete(target, receipt.Target, policy) {
			if err := st.AddMessageLabels(target.MessageID, labels); err != nil {
				return out, err
			}
			if err := st.RecordEmailHeadersContext(ctx, sourceID, target.MessageID, receipt.RFCID, receipt.Reply); err != nil {
				return out, err
			}
			out.kind = "unchanged"
			if recovered {
				out.kind = "updated"
			}
			return out, nil
		}
	}
	// A stale clean path must become pending before attempting new content.
	if item != nil && item.Status == "imported" {
		if err := st.PutEmlxLedgerItemContext(ctx, store.SourceImportItem{SourceID: sourceID, Provider: "emlx-occurrence", ProviderID: id, Name: rel, Checksum: item.Checksum, Status: "pending"}); err != nil {
			return out, err
		}
	}
	info, err := os.Stat(file)
	if err != nil {
		return out, err
	}
	if info.Size() > opts.MaxMessageBytes {
		return out, fmt.Errorf("file exceeds size limit %d", opts.MaxMessageBytes)
	}
	msg, err := io.parse(file, opts.MaxMessageBytes)
	if err != nil {
		return out, err
	}
	if emlx.IsPartial(filepath.Base(file)) {
		out.partial = 1
	}
	targetID := "emlx-" + msg.SourceHash
	states, err := st.EmlxTargetsContext(ctx, sourceID, []string{targetID})
	if err != nil {
		return out, err
	}
	target := states[targetID]
	if target.Deleted {
		out.kind = "skipped"
		return out, nil
	}
	all, err := emlxAllLabels(ctx, st, target.MessageID, labels)
	if err != nil {
		return out, err
	}
	var current []byte
	recovered := false
	if target.HasRaw {
		current, err = io.raw(ctx, st, target.MessageID)
		if err != nil {
			return out, err
		}
		if !targetComplete(target, targetID, policy) {
			// Finish the newest committed data, never overwrite it with older source
			// bytes just because this occurrence was acknowledged before the failure.
			if err := completeEmlxTarget(ctx, st, sourceID, targetID, policy, current, all, target.InternalDate.Time, opts, io, log); err != nil {
				return out, err
			}
			states, err = st.EmlxTargetsContext(ctx, sourceID, []string{targetID})
			if err != nil {
				return out, err
			}
			target = states[targetID]
			recovered = true
		}
	}
	// Source-part acknowledgments are independent of completion settings,
	// filesystem cache eligibility and which parts fit the current budget.
	var acknowledged map[string]string
	if target.HasRaw && decoded && receipt.Target == targetID {
		acknowledged = receipt.SourceParts
	}
	merged, err := emlx.MergeAttachments(msg.OriginalRaw, msg.Raw, current, msg.RestorationParts, opts.MaxMessageBytes, acknowledged)
	restorationErr := msg.RestorationError
	// An identical restored candidate already satisfies the source budget;
	// only differing archived parts need a separate combined-budget merge.
	if len(current) > 0 && len(msg.RestorationParts) > 0 && !bytes.Equal(msg.Raw, current) {
		merged, err = emlx.MergeAttachmentsFromFile(ctx, msg.OriginalRaw, current, file, opts.MaxMessageBytes, acknowledged)
		restorationErr = err
		// Preserve feasible progress even when another sibling could not be read.
		// The occurrence stays pending until all supported work succeeds.
		err = nil
	}
	if err != nil {
		return out, err
	}
	if len(current) == 0 && len(msg.RestorationParts) == 0 {
		merged.Raw = msg.Raw
	}
	rfc, reply := mime.ParseMessageIDs(msg.OriginalRaw)
	_, parseErr := mime.ParseWithRecovery(merged.Raw, "(MIME parse error)")
	needsIngest := !target.HasRaw || !bytes.Equal(current, merged.Raw) || opts.FullReconcile || reconcile || parseErr != nil
	if needsIngest {
		if err := completeEmlxTarget(ctx, st, sourceID, targetID, policy, merged.Raw, all, msg.PlistDate, opts, io, log); err != nil {
			return out, err
		}
		out.kind = "added"
		if target.MessageID != 0 {
			out.kind = "updated"
		}
		out.restored = int64(merged.ChangedParts)
	} else {
		if err := st.AddMessageLabels(target.MessageID, labels); err != nil {
			if err := completeEmlxTarget(ctx, st, sourceID, targetID, policy, current, all, msg.PlistDate, opts, io, log); err != nil {
				return out, err
			}
			out.kind = "updated"
		}
		if err := st.RecordEmailHeadersContext(ctx, sourceID, target.MessageID, rfc, reply); err != nil {
			return out, err
		}
		if out.kind == "" {
			out.kind = "skipped"
		}
		if recovered {
			out.kind = "updated"
		}
	}
	if fingerprintErr != nil {
		return out, errors.Join(fingerprintErr, restorationErr)
	}
	if restorationErr != nil {
		return out, fmt.Errorf("restore sibling attachments: %w", restorationErr)
	}
	if merged.Incomplete {
		return out, errors.New("attachment restoration exceeds current merge budget; retry with a larger --max-message-bytes limit (bytes, including MIME encoding)")
	}
	after, afterEligible, err := emlx.Fingerprint(ctx, file)
	if err != nil {
		return out, err
	}
	after = emlxDigest(fmt.Sprintf("%d:%s:%s:%d", emlxReceiptVersion, after, label, opts.MaxMessageBytes))
	if eligible && (!afterEligible || signature != after) {
		return out, errors.New("EMLX dependencies changed during import")
	}
	receipt = emlxReceipt{Version: emlxReceiptVersion, ID: id, SourceParts: merged.SourceParts, Target: targetID, RFCID: rfc, Reply: reply}
	if cacheAllowed && eligible && afterEligible {
		receipt.Signature = signature
	}
	checksum, err := encodeEmlxReceipt(receipt)
	if err != nil {
		return out, err
	}
	// Cold occurrences retain content evidence but have no metadata signature
	// authorizing a future content-read shortcut.
	return out, st.PutEmlxLedgerItemContext(ctx, store.SourceImportItem{SourceID: sourceID, Provider: "emlx-occurrence", ProviderID: id, Name: rel, Checksum: checksum, Size: info.Size(), Status: "imported"})
}
