package kataevidence

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/textutil"
)

// SourceReader loads live archive text. Load derives provenance for a
// selector; Read returns the text only while the reference still matches it.
type SourceReader interface {
	LoadKataEvidenceSource(ctx context.Context, selector Selector) (SourceRecord, error)
	ReadKataEvidenceSource(ctx context.Context, ref Reference) (SourceRecord, error)
}

type Service struct{ reader SourceReader }

func New(reader SourceReader) *Service { return &Service{reader: reader} }

// Prepare turns selectors into exact citations whose excerpts are the text a
// Kata issue saves.
func (s *Service) Prepare(ctx context.Context, selectors []Selector) ([]Evidence, error) {
	if len(selectors) < 1 || len(selectors) > MaxReferences {
		return nil, ErrInvalidReference
	}
	ranges := make([][2]int, len(selectors))
	for i, sel := range selectors {
		start, end, err := selectorRange(sel)
		if err != nil {
			return nil, err
		}
		ranges[i] = [2]int{start, end}
	}
	result := make([]Evidence, 0, len(selectors))
	for i, sel := range selectors {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := s.reader.LoadKataEvidenceSource(ctx, sel)
		if err != nil {
			return nil, ArchiveError(err)
		}
		if !utf8.ValidString(record.Text) {
			return nil, ErrChanged
		}
		start, end := ranges[i][0], ranges[i][1]
		if sel.Quote != "" {
			if start, end, err = quoteRange(record.Text, sel.Quote); err != nil {
				return nil, err
			}
		}
		textEnd := utf8.RuneCountInString(record.Text)
		if sel.MaxChars != nil {
			end = min(end, textEnd)
		}
		if end > textEnd || end <= start {
			return nil, ErrUnavailable
		}
		ref, err := Canonicalize(setRange(record.Reference, start, end))
		if err != nil {
			return nil, err
		}
		excerpt, _ := textutil.RuneSlice(record.Text, start, end)
		evidence := Evidence{ID: ID(ref), Passage: PassageID(ref, excerpt), Reference: ref, Excerpt: excerpt, Display: record.Display, ContentTrust: "untrusted"}
		if end < textEnd {
			evidence.NextRune = end
		}
		result = append(result, evidence)
	}
	return result, nil
}

// ResolveAround re-reads a canonical citation; callers canonicalize at the
// request boundary. A source that moved or disappeared is reported as a state
// rather than an error. When it is available, up to around runes of the cited
// representation's text are added on each side of the excerpt.
func (s *Service) ResolveAround(ctx context.Context, ref Reference, around int) (Resolution, error) {
	result := Resolution{Evidence: Evidence{ID: ID(ref), Reference: ref, ContentTrust: "untrusted"}}
	record, err := s.reader.ReadKataEvidenceSource(ctx, ref)
	if err != nil {
		switch {
		case errors.Is(err, ErrChanged):
			result.State = Changed
		case errors.Is(err, ErrUnavailable):
			result.State = Unavailable
		case errors.Is(err, ErrUnprocessed):
			result.State = Unprocessed
		case errors.Is(err, ErrUnsupported):
			result.State = Unsupported
		default:
			return Resolution{}, ArchiveError(err)
		}
		return result, nil
	}
	start, end := ref.Range()
	// Callers can hand-build references, so a range past the text is refused rather than shortened.
	excerpt, ok := textutil.RuneSlice(record.Text, start, end)
	if !utf8.ValidString(record.Text) || !ok {
		result.State = Changed
		return result, nil
	}
	result.State = Available
	result.Evidence.Excerpt, result.Evidence.Passage = excerpt, PassageID(ref, excerpt)
	result.Evidence.Display = record.Display
	if around > 0 {
		result.Before, _ = textutil.RuneSlice(record.Text, max(0, start-around), start)
		result.After, _ = textutil.RuneSlice(record.Text, end, min(utf8.RuneCountInString(record.Text), end+around))
	}
	return result, nil
}

func selectorRange(sel Selector) (int, int, error) {
	if sel.Quote != "" {
		if sel.StartRune != nil || sel.EndRune != nil || sel.MaxChars != nil || !utf8.ValidString(sel.Quote) || utf8.RuneCountInString(sel.Quote) > MaxChars {
			return 0, 0, ErrInvalidReference
		}
	} else if (sel.EndRune == nil) == (sel.MaxChars == nil) {
		return 0, 0, ErrInvalidReference
	}
	if sel.MessageID < 1 {
		return 0, 0, ErrInvalidReference
	}
	switch sel.Kind {
	case "message":
		if sel.AttachmentID != 0 || sel.ExtractionID != "" || sel.ChunkKey != "" {
			return 0, 0, ErrInvalidReference
		}
	case "document_chunk":
		if sel.AttachmentID < 1 || !validIdentity(sel.ExtractionID) || !validIdentity(sel.ChunkKey) {
			return 0, 0, ErrInvalidReference
		}
	default:
		return 0, 0, ErrInvalidReference
	}
	if sel.Quote != "" {
		// Prepare finds the range in the source text.
		return 0, 0, nil
	}
	start := 0
	if sel.StartRune != nil {
		start = *sel.StartRune
	}
	var size int
	if sel.EndRune != nil {
		size = *sel.EndRune - start
	} else {
		size = *sel.MaxChars
	}
	if start < 0 || size < 1 || size > MaxChars || start > math.MaxInt-size {
		return 0, 0, ErrInvalidReference
	}
	return start, start + size, nil
}

// quoteRange finds the single place quote appears in text, in runes.
func quoteRange(text, quote string) (int, int, error) {
	at := strings.Index(text, quote)
	if at < 0 {
		return 0, 0, ErrQuoteNotFound
	}
	if strings.Contains(text[at+1:], quote) {
		return 0, 0, ErrQuoteAmbiguous
	}
	start := utf8.RuneCountInString(text[:at])
	return start, start + utf8.RuneCountInString(quote), nil
}

func setRange(ref Reference, start, end int) Reference {
	switch {
	case ref.Kind == "message" && ref.Message != nil:
		p := *ref.Message
		p.StartRune, p.EndRune = start, end
		ref.Message = &p
	case ref.Kind == "document_chunk" && ref.DocumentChunk != nil:
		p := *ref.DocumentChunk
		p.StartRune, p.EndRune = start, end
		ref.DocumentChunk = &p
	}
	return ref
}

// ArchiveError keeps evidence outcomes recognizable and marks anything else
// as a failure to read the archive.
func ArchiveError(err error) error {
	for _, known := range []error{ErrInvalidReference, ErrUnavailable, ErrChanged, ErrUnprocessed, ErrUnsupported, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%w: %w", ErrArchiveUnavailable, err)
}
