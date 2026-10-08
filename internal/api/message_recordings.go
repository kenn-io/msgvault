package api

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
)

const (
	recordingReady        = "ready"
	recordingProcessing   = "processing"
	recordingMissing      = "missing"
	recordingFailed       = "failed"
	recordingUnsupported  = "unsupported"
	recordingMediaMissing = "media_missing"
	recordingUnavailable  = "unavailable"

	docbankEvidenceReady       = "ready"
	docbankEvidencePending     = "pending"
	docbankEvidenceUnavailable = "unavailable"

	suppliedTranscriptProfile = "supplied-transcript"
)

// messageRecordingDocbankBudget bounds every Docbank read for one request, so
// local states still come back when Docbank is slow.
var messageRecordingDocbankBudget = 20 * time.Second

// messageRecordingDocbankReads caps concurrent Docbank reads for one request.
const messageRecordingDocbankReads = 4

// MessageRecordingsResponse lists a message's live recordings.
type MessageRecordingsResponse struct {
	MessageID  int64              `json:"message_id"`
	Recordings []MessageRecording `json:"recordings"`
}

// MessageRecording is one recording on a message and its transcript state.
type MessageRecording struct {
	AttachmentID int64              `json:"attachment_id"`
	Filename     string             `json:"filename"`
	SizeBytes    int64              `json:"size_bytes"`
	State        string             `json:"state" enum:"ready,processing,missing,failed,unsupported,media_missing,unavailable"`
	Transcript   *MessageTranscript `json:"transcript,omitempty"`
}

// MessageTranscript is Docbank's transcript text for a ready recording.
type MessageTranscript struct {
	Origin  string                  `json:"origin" enum:"supplied,generated"`
	Partial bool                    `json:"partial"`
	Units   []MessageTranscriptUnit `json:"units"`
}

// MessageTranscriptUnit is one transcript line. Timing and speaker are set
// only when Docbank recorded them.
type MessageTranscriptUnit struct {
	Text    string `json:"text"`
	StartMS *int64 `json:"start_ms,omitempty"`
	EndMS   *int64 `json:"end_ms,omitempty"`
	Speaker string `json:"speaker,omitempty"`
}

// MessageRecordingReader joins local recordings with Docbank transcript
// evidence. Transcript text is read on demand and never stored.
type MessageRecordingReader struct {
	Store         *store.Store
	Client        *docbankmedia.Client
	Destination   string
	UploadConsent bool
}

func recordingKey(o store.MessageMediaOccurrence) string {
	if o.OccurrenceRef == "" {
		return strconv.FormatInt(o.AttachmentID, 10)
	}
	return o.OccurrenceRef + "\x00" + o.Revision
}

func (reader *MessageRecordingReader) list(
	ctx context.Context, messageID int64, logger *slog.Logger,
) ([]MessageRecording, error) {
	occurrences, err := reader.Store.ListMessageMediaOccurrences(ctx, reader.Destination, messageID)
	if err != nil {
		return nil, err
	}
	if len(occurrences) == 0 {
		return []MessageRecording{}, nil
	}
	recordings := reader.readAll(ctx, messageID, occurrences, logger)
	// A hide, delete or replacement during the remote reads discloses nothing.
	current, err := reader.Store.ListMessageMediaOccurrences(ctx, reader.Destination, messageID)
	if err != nil {
		return nil, err
	}
	fresh := make(map[string]store.MessageMediaOccurrence, len(current))
	for _, o := range current {
		fresh[recordingKey(o)] = o
	}
	kept := recordings[:0]
	for i, recording := range recordings {
		o, ok := fresh[recordingKey(occurrences[i])]
		if !ok {
			continue
		}
		if recording.State == recordingProcessing || (recording.Transcript != nil && recording.Transcript.Origin == "supplied") {
			recording = reader.recheckRevision(ctx, recording, o)
		}
		kept = append(kept, recording)
	}
	return kept, nil
}

// recheckRevision withholds text and processing labels when the current source cannot be proved.
func (reader *MessageRecordingReader) recheckRevision(
	ctx context.Context, recording MessageRecording, o store.MessageMediaOccurrence,
) MessageRecording {
	revision, err := beeper.MediaRevision(ctx, reader.Store, o.AttachmentID)
	if err != nil || revision != o.Revision {
		recording.State, recording.Transcript = recordingUnavailable, nil
	}
	return recording
}

// readAll reads Docbank evidence for every occurrence within one shared budget
// and logs failed reads once per request rather than once per recording.
func (reader *MessageRecordingReader) readAll(
	ctx context.Context, messageID int64, occurrences []store.MessageMediaOccurrence, logger *slog.Logger,
) []MessageRecording {
	remoteCtx, cancel := context.WithTimeout(ctx, docbankBudget(ctx))
	defer cancel()
	recordings := make([]MessageRecording, len(occurrences))
	readErrs := make([]error, len(occurrences))
	slots := make(chan struct{}, messageRecordingDocbankReads)
	var wg sync.WaitGroup
	for i, o := range occurrences {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			recordings[i], readErrs[i] = reader.recording(remoteCtx, o)
		})
	}
	wg.Wait()
	failed := 0
	code := ""
	for _, err := range readErrs {
		if err != nil {
			failed++
			code = cmp.Or(code, docbankmedia.ErrorCode(err))
		}
	}
	// A client that went away explains its own failed reads.
	if failed > 0 && ctx.Err() == nil {
		logger.Warn("read Docbank transcripts", "message_id", messageID, "failed", failed,
			"recordings", len(occurrences), "code", code)
	}
	return recordings
}

// docbankBudget leaves at least half the remaining request time for the local
// queries that follow the Docbank reads.
func docbankBudget(ctx context.Context) time.Duration {
	budget := messageRecordingDocbankBudget
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	return budget
}

// retryState labels planned transcription that the media worker may upload.
func (reader *MessageRecordingReader) retryState(o store.MessageMediaOccurrence) string {
	if reader.UploadConsent && o.DeliveryProfile != "" {
		return recordingProcessing
	}
	return recordingUnavailable
}

// recording labels one occurrence, reading Docbank only for retained media.
// The error reports a failed Docbank read, which leaves the recording unavailable.
func (reader *MessageRecordingReader) recording(
	ctx context.Context, o store.MessageMediaOccurrence,
) (MessageRecording, error) {
	result := MessageRecording{AttachmentID: o.AttachmentID, Filename: o.Filename, SizeBytes: o.Size, State: recordingUnavailable}
	if o.OccurrenceRef == "" {
		if o.AttachmentState == attachmentpolicy.StateSkipped {
			return result, nil
		}
		if !o.BytesArchived {
			result.State = recordingMediaMissing
		}
		return result, nil
	}
	switch o.RetentionState {
	case store.BeeperMediaRetentionRetained:
	case store.BeeperMediaRetentionPending, store.BeeperMediaRetentionSourceUnavailable:
		result.State = reader.retryState(o)
		return result, nil
	default:
		if o.ErrorCode == "unsupported_media" {
			result.State = recordingUnsupported
		}
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	transcript, err := reader.Client.Transcript(ctx, o.DocbankSourceID, o.SourceVersionID, o.ContentVersionID)
	if err != nil {
		return result, err
	}
	if transcript.VaultUID != o.VaultUID {
		return result, nil
	}
	switch transcript.EvidenceState {
	case docbankEvidenceReady:
		evidence := transcript.Transcript
		if evidence.Origin == "supplied" && !suppliedTranscriptMatches(o, evidence.SuppliedInputID) {
			result.State = reader.undeliveredTranscriptState(o)
			return result, nil
		}
		units := make([]MessageTranscriptUnit, 0, len(evidence.Units))
		for _, unit := range evidence.Units {
			out := MessageTranscriptUnit{Text: unit.Text, Speaker: unit.Speaker}
			if unit.TimeSpan != nil {
				start, end := unit.TimeSpan.StartMS, unit.TimeSpan.EndMS
				out.StartMS, out.EndMS = &start, &end
			}
			units = append(units, out)
		}
		result.State = recordingReady
		result.Transcript = &MessageTranscript{
			Origin:  evidence.Origin,
			Partial: evidence.Truncated || evidence.HasOmissions || evidence.Completeness == "partial",
			Units:   units,
		}
	case docbankEvidencePending:
		result.State = recordingProcessing
	case docbankEvidenceUnavailable:
		switch {
		case o.DeliveryPhase == "pending-artifact" || o.DeliveryPhase == "pending-process":
			result.State = reader.retryState(o)
		case o.DeliveryPhase == "blocked" && o.DeliveryOperationState == "":
			result.State = recordingUnavailable
		case transcript.OperationState == "failed" || transcript.OperationState == "cancelled":
			result.State = recordingFailed
		default:
			result.State = recordingMissing
		}
	}
	return result, nil
}

func suppliedTranscriptMatches(o store.MessageMediaOccurrence, inputID string) bool {
	return inputID != "" && o.SuppliedInputID == inputID
}

// undeliveredTranscriptState labels provider text msgvault cannot yet attribute
// to the message's current transcript.
func (reader *MessageRecordingReader) undeliveredTranscriptState(o store.MessageMediaOccurrence) string {
	switch {
	case o.DeliveryProfile != suppliedTranscriptProfile:
		return recordingUnavailable
	case o.DeliveryPhase == "done":
		if o.DeliveryOperationState == "failed" || o.DeliveryOperationState == "cancelled" {
			return recordingFailed
		}
		return recordingUnavailable
	case o.DeliveryPhase == "pending-artifact" || o.DeliveryPhase == "pending-process" ||
		o.DeliveryPhase == "observing":
		return reader.retryState(o)
	default:
		return recordingUnavailable
	}
}

func (s *Server) handleListMessageRecordings(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_id", "Message ID must be a positive integer")
		return
	}
	if s.messageRecordings == nil {
		writeJSON(w, http.StatusOK, MessageRecordingsResponse{MessageID: id, Recordings: []MessageRecording{}})
		return
	}
	recordings, err := s.messageRecordings.list(r.Context(), id, s.logger)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("list message recordings", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not read message recordings")
		return
	}
	writeJSON(w, http.StatusOK, MessageRecordingsResponse{MessageID: id, Recordings: recordings})
}
