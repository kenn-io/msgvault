package beeper

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/docbankmedia"
)

// readVoiceTranscripts reads a bounded page of completed transcripts each pass.
func (w *MediaSubmitter) readVoiceTranscripts(ctx context.Context) error {
	archiveUID, err := w.store.ArchiveUIDContext(ctx)
	if err != nil {
		return err
	}
	mappings, err := w.store.VoiceTranscriptMappings(ctx, w.destination, w.transcriptAfter, 8)
	if err != nil {
		return err
	}
	for _, mapping := range mappings {
		readCtx, cancel := context.WithTimeout(ctx, w.actionTimeout)
		transcript, err := w.transcriptClient.Transcript(readCtx, mapping.DocbankSourceID, mapping.SourceVersionID, mapping.ContentVersionID)
		cancel()
		if err == nil && transcript.VaultUID == mapping.VaultUID && transcript.EvidenceState == "ready" {
			text := generatedVoiceText(transcript)
			evidence, err := w.candidateEvidence(ctx, archiveUID, mapping.AttachmentID)
			if err != nil {
				return err
			}
			if !evidence.missing && evidence.gapCode == "" {
				err = w.gated(ctx, func() error {
					candidate, err := w.store.GetBeeperMediaCandidate(ctx, mapping.AttachmentID)
					if errors.Is(err, sql.ErrNoRows) {
						return nil
					}
					if err != nil {
						return err
					}
					_, rawHash, err := w.rawMediaEvidence(ctx, candidate.SourceType, candidate.MessageID)
					if errors.Is(err, errBeeperMediaRawInvalid) {
						return nil
					}
					if err != nil {
						return err
					}
					if !sameBeeperMediaCandidate(candidate, evidence.candidate) || rawHash != evidence.rawHash {
						return nil
					}
					if evidence.transcript != "" {
						return w.store.ReconcileBeeperMediaMapping(ctx, descriptorMapping(w.destination, candidate, evidence.descriptor))
					}
					return w.store.SaveVoiceTranscript(ctx, mapping, text)
				})
				if err != nil {
					return err
				}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.transcriptAfter = mapping.AttachmentID
	}
	if len(mappings) < 8 {
		w.transcriptAfter = 0
	}
	return nil
}

func generatedVoiceText(t docbankmedia.MediaTranscript) string {
	e := t.Transcript
	if e.Origin != "generated" || !docbankmedia.ValidSearchTranscript(e.Origin, e.SuppliedInputID, e.Completeness) {
		return ""
	}
	var text strings.Builder
	for _, unit := range e.Units {
		if strings.ContainsRune(unit.Text, 0) {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(unit.Text)
	}
	return text.String()
}
