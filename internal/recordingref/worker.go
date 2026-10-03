package recordingref

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
)

const referenceRequestTimeout = 30 * time.Second

type Worker struct {
	st          *store.Store
	client      *docbankmedia.Client
	destination string
	origins     []string
	logger      *slog.Logger
	gate        func(context.Context) (func(), bool)
}

func NewWorker(st *store.Store, client *docbankmedia.Client, destination string, origins []string, logger *slog.Logger) *Worker {
	canonical := make([]string, 0, len(origins))
	for _, raw := range origins {
		if origin, err := CanonicalOrigin(raw); err == nil {
			canonical = append(canonical, origin)
		}
	}
	slices.Sort(canonical)
	return &Worker{st: st, client: client, destination: destination, origins: slices.Compact(canonical), logger: logger}
}

func (w *Worker) WithOperationGate(gate func(context.Context) (func(), bool)) *Worker {
	w.gate = gate
	return w
}

func (w *Worker) gated(ctx context.Context, fn func() error) error {
	if w.gate != nil {
		release, ok := w.gate(ctx)
		if !ok {
			return errors.New("recording reference operation gate unavailable")
		}
		defer release()
	}
	return fn()
}

func messageRefs(m store.RecordingMessage, origins []string) []Ref {
	var refs []Ref
	seen := make(map[string]bool)
	anchors := ScanHTML(m.BodyHTML, origins)
	exact := make(map[string]bool, len(anchors))
	for _, r := range anchors {
		exact[r.Reference] = true
	}
	// Exact anchor targets take precedence over prose punctuation heuristics.
	for _, r := range append(anchors, scanProse(m.Body, origins, exact)...) {
		if !seen[r.RouteKey] {
			refs = append(refs, r)
			seen[r.RouteKey] = true
		}
	}
	for _, p := range m.Pointers {
		if r, ok := TeamsPointer(p.ID, p.Path); ok && !seen[r.RouteKey] {
			refs = append(refs, r)
			seen[r.RouteKey] = true
		}
	}
	return refs
}

func referenceInputs(m store.RecordingMessage, refs []Ref) ([]store.RecordingReferenceInput, error) {
	inputs := make([]store.RecordingReferenceInput, 0, len(refs))
	for _, r := range refs {
		sent := ""
		occ := docbankmedia.Occurrence{Ref: "msgvault:" + digest(strings.Join([]string{m.ArchiveUID, m.SourceType, m.SourceIdentifier, m.SourceMessageID, r.RouteKey}, "\x00"))}
		if m.SentAt != nil {
			sent = m.SentAt.UTC().Format(time.RFC3339Nano)
			occ.Message = docbankmedia.Timestamp{Normalized: sent, Raw: sent, Precision: "instant", Timezone: "UTC"}
		}
		occ.Revision = digest(r.ReferenceSHA256() + "\x00" + sent)
		encoded, err := json.Marshal(occ)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, store.RecordingReferenceInput{RouteKey: r.RouteKey, RefSHA256: r.ReferenceSHA256(), OccurrenceJSON: string(encoded)})
	}
	return inputs, nil
}

func (w *Worker) reconcile(ctx context.Context, m store.RecordingMessage) error {
	inputs, err := referenceInputs(m, messageRefs(m, w.origins))
	if err != nil {
		return err
	}
	return w.st.ReconcileRecordingReferences(ctx, w.destination, m.ID, m.Live, inputs)
}

func (w *Worker) RunBatch(ctx context.Context) error {
	var examined, pages, attempted int
	start := time.Now()
	policy := digest(strings.Join(w.origins, "\x00"))
	for pages < 50 && time.Since(start) < 20*time.Second {
		more := false
		err := w.gated(ctx, func() error {
			before, err := w.st.LoadRecordingReferenceCursor(ctx, w.destination)
			if err != nil {
				return err
			}
			cursor := before
			if cursor.Policy != policy {
				cursor = store.RecordingReferenceCursor{Policy: policy}
			}
			page, err := w.st.ListChangedMessages(ctx, cursor.FeedCursor(), 200)
			if err != nil {
				return err
			}
			for _, changed := range page.Messages {
				m, exists, err := w.st.ReadRecordingMessage(ctx, changed.ID)
				if err != nil {
					return err
				}
				if exists {
					if err := w.reconcile(ctx, m); err != nil {
						return err
					}
				}
				examined++
				cursor.At, cursor.AfterID, cursor.AfterRow = changed.ContentChangedAt, changed.ID, true
			}
			more = len(page.Messages) == 200
			if !more && !page.CompleteThrough.IsZero() {
				cursor.At, cursor.AfterID, cursor.AfterRow = page.CompleteThrough, 0, false
			}
			swapped, err := w.st.AdvanceRecordingReferenceCursor(ctx, w.destination, before, cursor)
			if err != nil {
				return err
			}
			if !swapped {
				more = false
			}
			return nil
		})
		if err != nil {
			return err
		}
		pages++
		if !more {
			break
		}
	}
	var claims []store.RecordingReferenceClaim
	if err := w.gated(ctx, func() error {
		var err error
		claims, err = w.st.ClaimRecordingReferences(ctx, w.destination, time.Now().UTC(), 20)
		return err
	}); err != nil {
		return err
	}
	for _, claim := range claims {
		if err := w.deliver(ctx, claim); err != nil {
			return err
		}
		attempted++
	}
	if w.logger != nil {
		w.logger.Debug("Recording reference pass", "examined", examined, "pages", pages, "attempted", attempted)
	}
	return nil
}

func (w *Worker) deliver(ctx context.Context, claim store.RecordingReferenceClaim) error {
	var m store.RecordingMessage
	var exists bool
	if err := w.gated(ctx, func() error {
		var err error
		m, exists, err = w.st.ReadRecordingMessage(ctx, claim.MessageID)
		return err
	}); err != nil {
		return err
	}
	var ref Ref
	rebuildable := false
	if exists && m.Live {
		for _, r := range messageRefs(m, w.origins) {
			if r.RouteKey == claim.RouteKey && r.ReferenceSHA256() == claim.RefSHA256 {
				ref, rebuildable = r, true
				break
			}
		}
	}
	now := time.Now().UTC()
	result := store.RecordingReferenceResult{State: claim.State, NextActionAt: now.Add(5 * time.Minute), LastSendAt: claim.LastSendAt, RetryCount: claim.RetryCount}
	var receipt docbankmedia.Receipt
	var err error
	requestCtx, cancel := context.WithTimeout(ctx, referenceRequestTimeout)
	defer cancel()
	recoverReceipt := claim.State == "uncertain" && (!rebuildable || strings.HasPrefix(claim.ErrorCode, "receipt_"))
	if claim.State == "uncertain" {
		switch claim.ErrorCode {
		case "unauthorized", "forbidden", "validation", "bad_request", "conflict", "not_found", "http_error":
			recoverReceipt = true
		}
	}
	switch {
	case rebuildable && !recoverReceipt:
		var occurrence docbankmedia.Occurrence
		if err := json.Unmarshal([]byte(claim.OccurrenceJSON), &occurrence); err != nil {
			return errors.New("invalid recording reference occurrence")
		}
		var marked bool
		if err := w.gated(ctx, func() error {
			var err error
			marked, err = w.st.MarkRecordingReferenceSending(ctx, claim, now)
			return err
		}); err != nil {
			return err
		}
		if !marked {
			return nil
		}
		result.State, result.LastSendAt = "uncertain", &now
		receipt, err = w.client.SubmitReference(requestCtx, docbankmedia.ReferenceRequest{OperationID: claim.OperationID, ReferenceURL: ref.Reference, CanonicalURL: ref.Canonical, Acquire: false, Occurrence: occurrence})
		if err == nil {
			result.State = "retained"
		} else {
			result.ErrorCode = docbankmedia.ErrorCode(err)
			result.RetryCount++
			httpErr, isHTTP := errors.AsType[*docbankmedia.HTTPError](err)
			switch {
			case errors.Is(err, docbankmedia.ErrCredentialUnavailable), errors.Is(err, docbankmedia.ErrInvalidRequest):
				result.LastSendAt = claim.LastSendAt
				if claim.State != "uncertain" {
					result.State = "pending"
				}
			case result.ErrorCode == "capability_unavailable":
				delay := 5 * time.Minute
				for n := 1; n < result.RetryCount && delay < time.Hour; n++ {
					delay *= 2
				}
				result.NextActionAt = now.Add(min(delay, time.Hour))
			case isHTTP && !httpErr.Retryable():
				if claim.State != "uncertain" {
					result.State = "blocked"
				}
			}
		}
	case recoverReceipt:
		receipt, err = w.client.OperationReceipt(requestCtx, claim.OperationID)
		if err == nil {
			result.State = "withdrawn"
			if rebuildable {
				result.State = "retained"
			}
		} else {
			result.ErrorCode = docbankmedia.ErrorCode(err)
			if !docbankmedia.IsNotFound(err) {
				result.ErrorCode = "receipt_" + result.ErrorCode
			}
			if docbankmedia.IsNotFound(err) && claim.LastSendAt != nil && !now.Before(claim.LastSendAt.Add(referenceRequestTimeout+5*time.Minute)) {
				result.State, result.ErrorCode = "withdrawn", "receipt_not_found"
				if rebuildable {
					result.State, result.NextActionAt = "pending", now
				}
			}
		}
	default:
		result.State = "withdrawn"
	}
	if err == nil {
		result.SourceID, result.OccurrenceID, result.Outcome, result.CoverageState = receipt.SourceID, receipt.OccurrenceID, receipt.Outcome, receipt.CoverageState
	}
	return w.gated(ctx, func() error {
		current, exists, err := w.st.ReadRecordingMessage(ctx, claim.MessageID)
		if err != nil {
			return err
		}
		inputs, err := referenceInputs(current, messageRefs(current, w.origins))
		if err != nil {
			return err
		}
		applied, err := w.st.FinishRecordingReference(ctx, claim, result, exists && current.Live, inputs)
		if err != nil {
			return err
		}
		if !applied {
			return nil
		}
		if w.logger != nil {
			w.logger.Info("Recording reference delivery", "message_id", claim.MessageID, "route_key", claim.RouteKey, "state", result.State, "error_code", result.ErrorCode, "source_id", result.SourceID, "occurrence_id", result.OccurrenceID, "outcome", result.Outcome, "coverage_state", result.CoverageState)
		}
		return nil
	})
}
