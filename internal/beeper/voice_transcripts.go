package beeper

import (
	"context"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/docbankmedia"
)

// readVoiceTranscripts reads a bounded page of completed transcripts each pass.
func (w *MediaSubmitter) readVoiceTranscripts(ctx context.Context) error {
	mappings, generation, err := w.store.VoiceTranscriptMappings(ctx, w.destination, w.transcriptAfter, 8)
	if err != nil {
		return err
	}
	for _, mapping := range mappings {
		readCtx, cancel := context.WithTimeout(ctx, w.actionTimeout)
		transcript, err := w.transcriptClient.Transcript(readCtx, mapping.DocbankSourceID, mapping.SourceVersionID, mapping.ContentVersionID)
		cancel()
		if err == nil && transcript.VaultUID == mapping.VaultUID {
			text, valid := generatedVoiceText(transcript)
			if valid {
				if err := w.gated(ctx, func() error { return w.store.SaveVoiceTranscript(ctx, mapping, generation, text) }); err != nil {
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

func generatedVoiceText(t docbankmedia.MediaTranscript) (string, bool) {
	e := t.Transcript
	if t.EvidenceState != "ready" || t.CoverageState != "transcribed" || e == nil || e.Origin != "generated" || e.BuildID == "" || !docbankmedia.ValidSearchTranscript(e.Origin, e.SuppliedInputID, e.Completeness) {
		return "", false
	}
	var text strings.Builder
	for _, unit := range e.Units {
		if strings.TrimSpace(unit.Text) == "" || !utf8.ValidString(unit.Text) || strings.ContainsRune(unit.Text, 0) {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(unit.Text)
		if text.Len() > beeperMediaTranscriptLimit {
			return "", false
		}
	}
	return text.String(), true
}
