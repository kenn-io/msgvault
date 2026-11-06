package agreements

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
)

func agreementAliasPair(t *testing.T, kind string) (kataevidence.Reference, kataevidence.Reference) {
	t.Helper()
	must := require.New(t)

	ref := validInput("target", "request", "beneficiary", "", 0, nil).Evidence[0]
	ref.MessageID = 99
	switch kind {
	case "document_chunk":
		ref.Kind, ref.Message, ref.AttachmentID, ref.OccurrenceKey = kind, nil, 8, "file-8"
		ref.DocumentChunk = &kataevidence.DocumentReference{CanonicalBlobHash: strings.Repeat("a", 64), ExtractionID: "extract-a", ManifestChecksum: strings.Repeat("b", 64), ChunkKey: "chunk-1", ChunkChecksum: strings.Repeat("c", 64), EndRune: 1}
	case "docbank_rendition":
		ref.Kind, ref.Message, ref.AttachmentID, ref.OccurrenceKey = kind, nil, 8, "file-8"
		ref.DocbankRendition = &kataevidence.DocbankReference{VaultUID: "vault", NodeID: 1, ContentVersionID: "content-a", ContentSHA256: strings.Repeat("a", 64), RenditionAttachmentID: strings.Repeat("b", 64), BuildID: strings.Repeat("c", 64), RenditionSHA256: strings.Repeat("d", 64), EndRune: 1}
	}
	original, err := kataevidence.Canonicalize(ref)
	must.NoError(err)
	alias, err := kataevidence.Canonicalize(original)
	must.NoError(err)
	switch kind {
	case "message":
		alias.Message.BodySHA256 = strings.Repeat("e", 64)
	case "document_chunk":
		alias.DocumentChunk.ExtractionID = "extract-b"
	case "docbank_rendition":
		alias.DocbankRendition.BuildID = strings.Repeat("e", 64)
	}
	return original, alias
}

func TestSourceVersionAliasCannotSupplySeparateAcceptance(t *testing.T) {
	terms := &Terms{Text: "Deliver", Obligor: "Alex", Beneficiary: "Sam"}
	for _, kind := range []string{"message", "document_chunk", "docbank_rendition"} {
		for _, action := range []string{"accept", "replace"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				check := assert.New(t)
				must := require.New(t)

				original, alias := agreementAliasPair(t, kind)
				request := validInput("r", "request", "beneficiary", "", 0, terms)
				if action == "accept" {
					request.Evidence = []kataevidence.Reference{original}
				}
				history, _, err := Append(nil, request, []string{"x"})
				must.NoError(err)
				target, count := "r", 1
				if action == "replace" {
					accept := validInput("a", "accept", "obligor", "r", 1, nil)
					history, _, err = Append(history.Events, accept, []string{"x"})
					must.NoError(err)
					proposal := validInput("p", "propose", "beneficiary", "a", 2, terms)
					proposal.Evidence = []kataevidence.Reference{original}
					history, _, err = Append(history.Events, proposal, []string{"x"})
					must.NoError(err)
					target, count = "p", 3
				}
				acceptance := validInput("new-acceptance", action, "obligor", target, count, nil)
				acceptance.Evidence = []kataevidence.Reference{alias}
				_, _, err = Append(history.Events, acceptance, []string{"x"})
				must.ErrorIs(err, ErrInvalid, "a reprocessed reference to the same words is not separate acceptance")
				// A real text edit at the same location can supply a new passage.
				_, view, err := Append(history.Events, acceptance, []string{"y"})
				must.NoError(err)
				check.Equal(acceptance.ID, view.AcceptedEventID)
			})
		}
	}
}
