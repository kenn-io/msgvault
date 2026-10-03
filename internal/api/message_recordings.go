package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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
)

// messageRecordingDocbankBudget bounds every Docbank read for one request,
// well under the request deadline, so local states still come back.
var messageRecordingDocbankBudget = 20 * time.Second

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
	remoteCtx, cancel := context.WithTimeout(ctx, messageRecordingDocbankBudget)
	defer cancel()
	recordings := make([]MessageRecording, 0, len(occurrences))
	keys := make([]string, 0, len(occurrences))
	for _, o := range occurrences {
		recordings = append(recordings, reader.recording(remoteCtx, o, logger))
		keys = append(keys, recordingKey(o))
	}
	// A hide, delete or replacement during the remote reads discloses nothing.
	current, err := reader.Store.ListMessageMediaOccurrences(ctx, reader.Destination, messageID)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(current))
	for _, o := range current {
		live[recordingKey(o)] = true
	}
	kept := recordings[:0]
	for i, recording := range recordings {
		if !live[keys[i]] {
			continue
		}
		// An edited provider transcript keeps the audio but changes the revision,
		// which discovery then sends again.
		if recording.Transcript != nil && recording.Transcript.Origin == "supplied" {
			revision, err := beeper.MediaRevision(ctx, reader.Store, occurrences[i].AttachmentID)
			if err != nil || revision != occurrences[i].Revision {
				recording.State, recording.Transcript = reader.retryState(), nil
			}
		}
		kept = append(kept, recording)
	}
	return kept, nil
}

// retryState labels a recording the media worker will send again, which it
// does only while it may upload.
func (reader *MessageRecordingReader) retryState() string {
	if reader.UploadConsent {
		return recordingProcessing
	}
	return recordingUnavailable
}

func (reader *MessageRecordingReader) recording(
	ctx context.Context, o store.MessageMediaOccurrence, logger *slog.Logger,
) MessageRecording {
	result := MessageRecording{AttachmentID: o.AttachmentID, Filename: o.Filename, SizeBytes: o.Size, State: recordingUnavailable}
	if o.OccurrenceRef == "" {
		result.State = recordingMediaMissing
		return result
	}
	switch o.RetentionState {
	case store.BeeperMediaRetentionRetained:
	case store.BeeperMediaRetentionPending, store.BeeperMediaRetentionSourceUnavailable:
		result.State = reader.retryState()
		return result
	default:
		if o.ErrorCode == "unsupported_media" {
			result.State = recordingUnsupported
		}
		return result
	}
	if ctx.Err() != nil {
		return result
	}
	transcript, err := reader.Client.Transcript(ctx, o.DocbankSourceID, o.SourceVersionID, o.ContentVersionID)
	if err != nil {
		logger.Warn("read Docbank transcript", "attachment_id", o.AttachmentID, "code", docbankmedia.ErrorCode(err))
		return result
	}
	if transcript.VaultUID != o.VaultUID {
		return result
	}
	switch transcript.EvidenceState {
	case docbankEvidenceReady:
		evidence := transcript.Transcript
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
		case transcript.OperationState == "failed" || transcript.OperationState == "cancelled":
			result.State = recordingFailed
		case reader.UploadConsent &&
			(o.DeliveryPhase == "pending-artifact" || o.DeliveryPhase == "pending-process"):
			// Msgvault has not yet handed this recording's transcript work to Docbank.
			result.State = recordingProcessing
		default:
			result.State = recordingMissing
		}
	}
	return result
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
