package beeper

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/textutil"
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
			err = w.gated(ctx, func() error {
				evidence, err := w.candidateEvidence(ctx, archiveUID, mapping.AttachmentID)
				if err != nil {
					return err
				}
				if evidence.missing || evidence.gapCode != "" {
					return nil
				}
				if evidence.transcript != "" {
					return w.store.ReconcileBeeperMediaMapping(ctx, descriptorMapping(w.destination, evidence.candidate, evidence.descriptor))
				}
				return w.store.SaveVoiceTranscript(ctx, mapping, text)
			})
			if err != nil {
				return err
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
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(textutil.ReplaceNUL(unit.Text))
	}
	return text.String()
}
