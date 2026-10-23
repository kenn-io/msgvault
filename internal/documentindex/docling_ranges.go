package documentindex

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/msgvault/internal/store"
)

const doclingPDFRangePages = 50

func (w *DoclingWorker) renderPageRanges(ctx context.Context, content []byte, candidate store.DocumentExtractionCandidate, format mistral.CandidateFormat, claim store.DocumentExtractionClaim, pages int, accounting *documentPublicationAccounting) (document.SourceDocument, error) {
	checkpoints, err := w.catalog.LoadDocumentExtractionRanges(ctx, claim, w.config.Documents.MaxResponseBytes)
	if err != nil {
		return document.SourceDocument{}, fmt.Errorf("%w: %w", errDocumentRangeCheckpoint, err)
	}
	saved := make(map[int]store.DocumentExtractionRange, len(checkpoints))
	for _, r := range checkpoints {
		if r.TotalPages != pages || (r.FirstPage-1)%doclingPDFRangePages != 0 || r.LastPage != min(r.FirstPage+doclingPDFRangePages-1, r.TotalPages) {
			return document.SourceDocument{}, errors.New("document range checkpoint has inconsistent page bounds")
		}
		saved[r.FirstPage] = r
	}
	source := document.SourceDocument{Family: "pdf", UnitKind: "page"}
	var markdown []string
	degraded := false
	for first := 1; first <= pages; first += doclingPDFRangePages {
		last := min(first+doclingPDFRangePages-1, pages)
		checkpoint, ok := saved[first]
		var units []document.SourceUnit
		var fallback bool
		if !ok {
			upload, authorization, err := w.authorizeUpload(content, candidate, format, claim)
			if err != nil {
				return document.SourceDocument{}, err
			}
			authorization.RenditionRequestFingerprint, err = documentIdentityChecksum(struct {
				Extraction  string
				First, Last int
				Capability  string
			}{claim.ExtractionID, first, last, authorization.CapabilityRecordChecksum})
			if err != nil {
				return document.SourceDocument{}, err
			}
			started := time.Now()
			rendered, err := w.provider.RenderPageRange(ctx, upload, authorization, docling.PageRange{First: int64(first), Last: int64(last)})
			accounting.Latency += time.Since(started)
			if err != nil {
				return document.SourceDocument{}, fmt.Errorf("render Docling page range: %w", err)
			}
			accounting.Requests += int(rendered.Receipt.Usage.Requests)
			accounting.Retries += int(rendered.Receipt.Usage.Retries)
			if slices.Contains(rendered.Receipt.Warnings, "partial_success") {
				return document.SourceDocument{}, errors.New("docling returned a partial result")
			}
			// Canonical evidence retains the selection marker and any degraded provenance.
			// Its text limit covers the byte budget; final document truncation happens once.
			policy, err := document.NewEvidencePolicy(int(w.config.Documents.MaxResponseBytes))
			if err != nil {
				return document.SourceDocument{}, fmt.Errorf("configure range evidence: %w", err)
			}
			evidence, err := document.NormalizeEvidenceV1(rendered.Evidence, policy)
			if err != nil {
				return document.SourceDocument{}, fmt.Errorf("normalize range evidence: %w", err)
			}
			encoded, checksum, err := document.MarshalNormalizedEvidenceV1(evidence)
			if err != nil {
				return document.SourceDocument{}, fmt.Errorf("encode range evidence: %w", err)
			}
			checkpoint = store.DocumentExtractionRange{FirstPage: first, LastPage: last, TotalPages: pages, Evidence: encoded, EvidenceChecksum: checksum, Markdown: string(rendered.ProviderMarkdown)}
			// Validate before saving so an incomplete selection can be retried.
			units, fallback, err = doclingEvidenceRangeUnits(evidence, checkpoint)
			if err != nil {
				return document.SourceDocument{}, err
			} else if fallback && strings.TrimSpace(checkpoint.Markdown) == "" {
				return document.SourceDocument{}, errors.New("docling range has no bounded Markdown fallback")
			}
			if !fallback {
				checkpoint.Markdown = ""
			}
			checkpoint.CharCount = utf8.RuneCountInString(checkpoint.Markdown)
			for _, unit := range evidence.Units {
				checkpoint.CharCount += utf8.RuneCountInString(unit.Text)
			}
			if err := w.catalog.SaveDocumentExtractionRange(ctx, claim, checkpoint, w.config.Documents.MaxResponseBytes); err != nil {
				return document.SourceDocument{}, fmt.Errorf("%w: %w", errDocumentRangeCheckpoint, err)
			}
		} else {
			units, fallback, err = doclingRangeUnits(checkpoint)
			if err != nil {
				return document.SourceDocument{}, err
			}
		}
		degraded = degraded || fallback
		source.Units = append(source.Units, units...)
		rangeMarkdown := checkpoint.Markdown
		if strings.TrimSpace(rangeMarkdown) == "" {
			if fallback {
				return document.SourceDocument{}, errors.New("docling range has no bounded Markdown fallback")
			}
			var text []string
			for _, unit := range units {
				text = append(text, unit.Markdown)
			}
			rangeMarkdown = strings.Join(text, "\n\n")
		}
		markdown = append(markdown, rangeMarkdown)
	}
	accounting.UnitsProcessed = pages
	if degraded {
		combined := strings.Join(markdown, "\n\n")
		if strings.TrimSpace(combined) == "" {
			return document.SourceDocument{}, errors.New("docling ranges have no bounded Markdown fallback")
		}
		source.UnitKind = "document"
		source.Units = []document.SourceUnit{{Index: 0, Markdown: combined}}
	}
	return source, nil
}

func doclingRangeUnits(r store.DocumentExtractionRange) ([]document.SourceUnit, bool, error) {
	var evidence document.NormalizedEvidenceV1
	if err := json.Unmarshal(r.Evidence, &evidence); err != nil {
		return nil, false, fmt.Errorf("decode document range evidence: %w", err)
	}
	// Revalidate canonical structure before consuming a durable checkpoint.
	canonical, checksum, err := document.MarshalNormalizedEvidenceV1(evidence)
	if err != nil || checksum != r.EvidenceChecksum || !bytes.Equal(canonical, r.Evidence) {
		return nil, false, errors.New("invalid canonical document range evidence")
	}
	return doclingEvidenceRangeUnits(evidence, r)
}

func doclingEvidenceRangeUnits(evidence document.NormalizedEvidenceV1, r store.DocumentExtractionRange) ([]document.SourceUnit, bool, error) {
	degraded := evidence.UnitKind != document.EvidenceUnitPage || evidence.Family != "pdf"
	for _, omission := range evidence.Omissions {
		if omission.Kind == document.EvidenceOmissionField && omission.Field == "page_range" {
			continue
		}
		if omission.Kind == document.EvidenceOmissionUnit && omission.Reason == "pages precede the requested range" && omission.Locator != nil && omission.Locator.Start == 1 && omission.Locator.End == int64(r.FirstPage-1) {
			continue
		}
		degraded = true
	}
	if degraded {
		return nil, true, nil
	}
	if len(evidence.Units) != r.LastPage-r.FirstPage+1 {
		return nil, false, errors.New("docling range has incomplete page coverage")
	}
	units := make([]document.SourceUnit, 0, len(evidence.Units))
	for i, unit := range evidence.Units {
		page := int64(r.FirstPage + i)
		if unit.Locator.Kind != document.EvidenceLocatorPage || unit.Locator.Start != page || unit.Locator.End != page {
			return nil, false, errors.New("docling range has invalid original page locators")
		}
		if len(unit.Omissions) > 0 {
			degraded = true
		}
		units = append(units, document.SourceUnit{Index: int(page) - 1, Markdown: unit.Text})
	}
	return units, degraded, nil
}
