package documentindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"
)

const (
	normalizationPolicyVersion = 2
	headingSentinelStart       = '\ue000'
	headingSentinelEnd         = '\ue001'
)

type SourceDocument struct {
	Family   string
	UnitKind string
	Units    []SourceUnit
}

type SourceUnit struct {
	Index      int
	Markdown   string
	Header     string
	Footer     string
	Dimensions UnitDimensions
}

type UnitDimensions struct {
	DPI    int
	Height int
	Width  int
}

type NormalizePolicy struct {
	MaxUnitChars           int
	MaxDocumentChars       int
	MaxSourceUnitBytes     int
	MaxMetadataSourceBytes int
	MaxLinkChars           int
	MaxChunkRunes          int
	ChunkOverlap           int
	MaxChunks              int
}

func DefaultNormalizePolicy(maxDocumentChars int) NormalizePolicy {
	return NormalizePolicy{
		MaxUnitChars: 1_000_000, MaxDocumentChars: maxDocumentChars,
		MaxSourceUnitBytes: 4_000_000, MaxMetadataSourceBytes: 65_536, MaxLinkChars: 2_048,
		MaxChunkRunes: 4_000, ChunkOverlap: 200, MaxChunks: 20_000,
	}
}

type NormalizedDocument struct {
	PolicyVersion int
	Family        string
	UnitKind      string
	Units         []NormalizedUnit
	Chunks        []DocumentChunk
	Checksum      string
	Truncated     bool
}

type NormalizedUnit struct {
	Index        int
	SourceKey    string
	Kind         string
	Text         string
	Header       string
	Footer       string
	Dimensions   UnitDimensions
	CharCount    int
	Checksum     string
	Truncated    bool
	HeadingMarks []HeadingMark
}

type HeadingMark struct {
	CharOffset int
	Path       []string
}

type DocumentChunk struct {
	Key         string
	Ordinal     int
	Text        string
	HeadingPath []string
	CharCount   int
	Checksum    string
	Truncated   bool
	Spans       []DocumentChunkSpan
}

type DocumentChunkSpan struct {
	UnitIndex int
	CharStart int
	CharEnd   int
}

// NormalizeDocument converts transient provider Markdown into deterministic,
// inert canonical text plus exact unit spans. It never retains raw responses.
func NormalizeDocument(source SourceDocument, policy NormalizePolicy) (NormalizedDocument, error) {
	if source.Family == "" || source.UnitKind == "" || len(source.Units) == 0 {
		return NormalizedDocument{}, errors.New("document normalization requires family, unit kind, and units")
	}
	if err := validateNormalizePolicy(policy); err != nil {
		return NormalizedDocument{}, err
	}

	result := NormalizedDocument{
		PolicyVersion: normalizationPolicyVersion, Family: source.Family, UnitKind: source.UnitKind,
		Units: make([]NormalizedUnit, 0, len(source.Units)),
	}
	remaining := policy.MaxDocumentChars
	for i, unit := range source.Units {
		if unit.Index != i {
			return NormalizedDocument{}, fmt.Errorf("document source unit %d has noncontiguous index %d", i, unit.Index)
		}
		if unit.Dimensions.DPI < 0 || unit.Dimensions.Height < 0 || unit.Dimensions.Width < 0 ||
			unit.Dimensions.DPI > 100_000 || unit.Dimensions.Height > 10_000_000 || unit.Dimensions.Width > 10_000_000 {
			return NormalizedDocument{}, fmt.Errorf("document source unit %d has invalid dimensions", i)
		}
		text, headings, sourceTruncated, err := canonicalMarkdown(
			unit.Markdown, policy.MaxLinkChars, policy.MaxSourceUnitBytes,
		)
		if err != nil {
			return NormalizedDocument{}, fmt.Errorf("normalize document source unit %d: %w", i, err)
		}
		header, _, headerSourceTruncated, err := canonicalMarkdown(
			unit.Header, policy.MaxLinkChars, policy.MaxMetadataSourceBytes,
		)
		if err != nil {
			return NormalizedDocument{}, fmt.Errorf("normalize document source unit %d header: %w", i, err)
		}
		footer, _, footerSourceTruncated, err := canonicalMarkdown(
			unit.Footer, policy.MaxLinkChars, policy.MaxMetadataSourceBytes,
		)
		if err != nil {
			return NormalizedDocument{}, fmt.Errorf("normalize document source unit %d footer: %w", i, err)
		}
		unitTruncated := sourceTruncated || headerSourceTruncated || footerSourceTruncated
		header, truncated := truncateRunes(header, min(policy.MaxUnitChars, 16_384))
		unitTruncated = unitTruncated || truncated
		footer, truncated = truncateRunes(footer, min(policy.MaxUnitChars, 16_384))
		unitTruncated = unitTruncated || truncated
		text, bodyOffset := joinDocumentUnitEvidence(header, text, footer)
		for headingIndex := range headings {
			headings[headingIndex].CharOffset += bodyOffset
		}
		text, truncated = truncateRunes(text, min(policy.MaxUnitChars, remaining))
		unitTruncated = unitTruncated || truncated
		if truncated {
			result.Truncated = true
		}
		combinedChars := utf8.RuneCountInString(text)
		if combinedChars == 0 && remaining == 0 {
			result.Truncated = true
			break
		}
		remaining -= combinedChars
		headings = boundHeadingMarks(text, headings)
		normalized := NormalizedUnit{
			Index: unit.Index, SourceKey: fmt.Sprintf("%s:%06d", source.UnitKind, unit.Index), Kind: source.UnitKind,
			Text: text, Header: header, Footer: footer, Dimensions: unit.Dimensions,
			CharCount: utf8.RuneCountInString(text), Truncated: unitTruncated, HeadingMarks: headings,
		}
		normalized.Checksum = checksumStrings(normalized.SourceKey, normalized.Text, normalized.Header, normalized.Footer)
		result.Units = append(result.Units, normalized)
		result.Truncated = result.Truncated || unitTruncated
	}
	if len(result.Units) == 0 {
		return NormalizedDocument{}, errors.New("document normalization produced no units")
	}

	chunks, chunksTruncated := chunkNormalizedUnits(result.Units, policy)
	result.Chunks = chunks
	result.Truncated = result.Truncated || chunksTruncated
	checksumParts := []string{fmt.Sprintf("v%d", result.PolicyVersion), result.Family, result.UnitKind}
	for _, unit := range result.Units {
		checksumParts = append(checksumParts, unit.Checksum)
	}
	for _, chunk := range result.Chunks {
		checksumParts = append(checksumParts, chunk.Checksum)
	}
	result.Checksum = checksumStrings(checksumParts...)
	return result, nil
}

func joinDocumentUnitEvidence(header, body, footer string) (string, int) {
	parts := make([]string, 0, 3)
	bodyOffset := 0
	if header != "" {
		parts = append(parts, header)
		bodyOffset = utf8.RuneCountInString(header)
		if body != "" || footer != "" {
			bodyOffset += 2
		}
	}
	if body != "" {
		parts = append(parts, body)
	}
	if footer != "" {
		parts = append(parts, footer)
	}
	return strings.Join(parts, "\n\n"), bodyOffset
}

func validateNormalizePolicy(policy NormalizePolicy) error {
	if policy.MaxUnitChars <= 0 || policy.MaxDocumentChars <= 0 || policy.MaxSourceUnitBytes <= 0 ||
		policy.MaxMetadataSourceBytes <= 0 || policy.MaxLinkChars <= 0 ||
		policy.MaxChunkRunes <= 0 || policy.MaxChunks <= 0 {
		return errors.New("document normalization limits must be positive")
	}
	if policy.ChunkOverlap < 0 || policy.ChunkOverlap >= policy.MaxChunkRunes {
		return errors.New("document normalization limits are inconsistent")
	}
	return nil
}

func canonicalMarkdown(markdown string, maxLinkChars, maxSourceBytes int) (string, []HeadingMark, bool, error) {
	if markdown == "" {
		return "", nil, false, nil
	}
	if !utf8.ValidString(markdown) {
		return "", nil, false, errors.New("provider Markdown is invalid UTF-8")
	}
	markdown, sourceTruncated := truncateUTF8Bytes(markdown, maxSourceBytes)
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM))
	var rendered bytes.Buffer
	if err := parser.Convert([]byte(markdown), &rendered); err != nil {
		return "", nil, false, fmt.Errorf("parse provider Markdown: %w", err)
	}
	writer := canonicalHTMLWriter{maxLinkChars: maxLinkChars}
	if err := writer.consume(bytes.NewReader(rendered.Bytes())); err != nil {
		return "", nil, false, err
	}
	text, headings := canonicalWhitespace(writer.output.String())
	return text, headings, sourceTruncated, nil
}

type canonicalHTMLWriter struct {
	output       strings.Builder
	maxLinkChars int
	inPre        bool
	skipDepth    int
	cellIndex    int
	links        []string
	preFenceOpen bool
	pendingSpace bool
}

func (w *canonicalHTMLWriter) consume(reader io.Reader) error {
	tokenizer := html.NewTokenizer(reader)
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if errors.Is(tokenizer.Err(), io.EOF) {
				return nil
			}
			return fmt.Errorf("tokenize normalized document HTML: %w", tokenizer.Err())
		case html.TextToken:
			if w.skipDepth == 0 {
				w.writeText(string(tokenizer.Text()))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			w.startTag(token)
		case html.EndTagToken:
			w.endTag(tokenizer.Token().Data)
		case html.CommentToken, html.DoctypeToken:
			// Comments and document declarations are not searchable evidence.
		}
	}
}

func (w *canonicalHTMLWriter) startTag(token html.Token) {
	tag := token.Data
	if w.skipDepth > 0 {
		w.skipDepth++
		return
	}
	if tag == "script" || tag == "style" || tag == "svg" {
		w.skipDepth = 1
		return
	}
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		w.block()
		level := int(tag[1] - '0')
		_, _ = fmt.Fprintf(&w.output, "%cH%d%c", headingSentinelStart, level, headingSentinelEnd)
		w.output.WriteString(strings.Repeat("#", level) + " ")
	case "p", "div", "section", "blockquote", "ul", "ol", "table":
		w.block()
	case "li":
		w.line()
		w.output.WriteString("- ")
	case "br":
		w.line()
	case "tr":
		w.line()
		w.cellIndex = 0
	case "td", "th":
		if w.cellIndex > 0 {
			w.output.WriteString(" | ")
		}
		w.cellIndex++
	case "pre":
		w.block()
		w.output.WriteString("```")
		w.inPre = true
		w.preFenceOpen = true
	case "code":
		if w.inPre && w.preFenceOpen {
			for _, attribute := range token.Attr {
				if attribute.Key == "class" && strings.HasPrefix(attribute.Val, "language-") {
					language := strings.TrimPrefix(attribute.Val, "language-")
					if language != "" && len(language) <= 64 {
						w.output.WriteString(language)
					}
				}
			}
			w.output.WriteByte('\n')
			w.preFenceOpen = false
		} else if !w.inPre {
			w.output.WriteByte('`')
		}
	case "img":
		for _, attribute := range token.Attr {
			if attribute.Key == "alt" {
				w.writeText(attribute.Val)
				break
			}
		}
	case "input":
		isCheckbox := false
		checked := false
		for _, attribute := range token.Attr {
			if attribute.Key == "type" && attribute.Val == "checkbox" {
				isCheckbox = true
			}
			if attribute.Key == "checked" {
				checked = true
			}
		}
		if isCheckbox {
			if checked {
				w.output.WriteString("[x] ")
			} else {
				w.output.WriteString("[ ] ")
			}
		}
	case "a":
		link := ""
		for _, attribute := range token.Attr {
			if attribute.Key == "href" {
				link = safeStoredLink(attribute.Val, w.maxLinkChars)
				break
			}
		}
		w.links = append(w.links, link)
	}
}

func (w *canonicalHTMLWriter) endTag(tag string) {
	if w.skipDepth > 0 {
		w.skipDepth--
		return
	}
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6", "p", "div", "section", "blockquote", "ul", "ol", "table":
		w.block()
	case "li", "tr":
		w.line()
	case "pre":
		if w.preFenceOpen {
			w.output.WriteByte('\n')
		}
		w.inPre = false
		w.preFenceOpen = false
		w.line()
		w.output.WriteString("```")
		w.block()
	case "code":
		if !w.inPre {
			w.output.WriteByte('`')
		}
	case "a":
		if len(w.links) == 0 {
			return
		}
		link := w.links[len(w.links)-1]
		w.links = w.links[:len(w.links)-1]
		if link != "" {
			w.output.WriteString(" (" + link + ")")
		}
	}
}

func (w *canonicalHTMLWriter) writeText(value string) {
	if w.inPre {
		if w.preFenceOpen {
			w.output.WriteByte('\n')
			w.preFenceOpen = false
		}
		w.output.WriteString(stripUnsafeControls(value, true))
		return
	}
	value = stripUnsafeControls(value, false)
	for _, character := range value {
		if unicode.IsSpace(character) {
			w.pendingSpace = true
			continue
		}
		if w.pendingSpace && w.output.Len() > 0 &&
			!strings.HasSuffix(w.output.String(), "\n") && !strings.HasSuffix(w.output.String(), " ") {
			w.output.WriteByte(' ')
		}
		w.pendingSpace = false
		w.output.WriteRune(character)
	}
}

func (w *canonicalHTMLWriter) line() {
	w.pendingSpace = false
	if w.output.Len() > 0 && !strings.HasSuffix(w.output.String(), "\n") {
		w.output.WriteByte('\n')
	}
}

func (w *canonicalHTMLWriter) block() {
	w.line()
	if w.output.Len() > 0 && !strings.HasSuffix(w.output.String(), "\n\n") {
		w.output.WriteByte('\n')
	}
}

func stripUnsafeControls(value string, preserveNewlines bool) string {
	return strings.Map(func(character rune) rune {
		if character == '\n' && preserveNewlines || character == '\t' {
			return character
		}
		if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' ||
			character == headingSentinelStart || character == headingSentinelEnd {
			return -1
		}
		return character
	}, strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n"))
}

func safeStoredLink(value string, maxChars int) string {
	if utf8.RuneCountInString(value) > maxChars {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return ""
	}
	return parsed.String()
}

func canonicalWhitespace(value string) (string, []HeadingMark) {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	var output strings.Builder
	headings := make([]HeadingMark, 0)
	headingPath := make([]string, 0, 6)
	blank := false
	endsWithNewline := false
	runeOffset := 0
	writeNewline := func() {
		output.WriteByte('\n')
		endsWithNewline = true
		runeOffset++
	}
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if output.Len() == 0 || blank {
				continue
			}
			blank = true
			writeNewline()
			continue
		}
		level := 0
		prefix := string(headingSentinelStart) + "H"
		if strings.HasPrefix(line, prefix) {
			end := strings.IndexRune(line, headingSentinelEnd)
			if end == len(prefix)+1 && line[len(prefix)] >= '1' && line[len(prefix)] <= '6' {
				level = int(line[len(prefix)] - '0')
				line = line[end+utf8.RuneLen(headingSentinelEnd):]
			}
		}
		blank = false
		if output.Len() > 0 && !endsWithNewline {
			writeNewline()
		}
		offset := runeOffset
		output.WriteString(line)
		runeOffset += utf8.RuneCountInString(line)
		endsWithNewline = false
		if level > 0 {
			for len(headingPath) < level {
				headingPath = append(headingPath, "")
			}
			headingPath = headingPath[:level]
			headingPath[level-1] = strings.TrimSpace(strings.TrimLeft(line, "#"))
			headings = append(headings, HeadingMark{
				CharOffset: offset,
				Path:       append([]string(nil), compactHeadingPath(headingPath)...),
			})
		}
	}
	return strings.TrimRight(output.String(), "\n"), headings
}

func boundHeadingMarks(text string, headings []HeadingMark) []HeadingMark {
	textRunes := []rune(text)
	bounded := make([]HeadingMark, 0, len(headings))
	for _, heading := range headings {
		if heading.CharOffset < 0 || heading.CharOffset >= len(textRunes) || len(heading.Path) == 0 {
			continue
		}
		end := heading.CharOffset
		for end < len(textRunes) && textRunes[end] != '\n' {
			end++
		}
		title := strings.TrimSpace(strings.TrimLeft(string(textRunes[heading.CharOffset:end]), "#"))
		path := append([]string(nil), heading.Path...)
		path[len(path)-1] = title
		bounded = append(bounded, HeadingMark{CharOffset: heading.CharOffset, Path: path})
	}
	return bounded
}

func truncateUTF8Bytes(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}

func truncateRunes(value string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	for byteOffset := range value {
		if limit == 0 {
			return value[:byteOffset], true
		}
		limit--
	}
	return value, false
}

func chunkNormalizedUnits(units []NormalizedUnit, policy NormalizePolicy) ([]DocumentChunk, bool) {
	chunks := make([]DocumentChunk, 0)
	truncated := false
	for _, unit := range units {
		spans := chunkUnitText(unit.Text, policy.MaxChunkRunes, policy.ChunkOverlap)
		for _, span := range spans {
			if len(chunks) >= policy.MaxChunks {
				return chunks, true
			}
			chunk := DocumentChunk{
				Key:     fmt.Sprintf("%s:%06d-%06d", unit.SourceKey, span.CharStart, span.CharEnd),
				Ordinal: len(chunks), Text: span.Text, HeadingPath: headingPathAt(unit.HeadingMarks, span.CharStart),
				CharCount: utf8.RuneCountInString(span.Text), Truncated: unit.Truncated,
				Spans: []DocumentChunkSpan{{UnitIndex: unit.Index, CharStart: span.CharStart, CharEnd: span.CharEnd}},
			}
			chunk.Checksum = checksumStrings(chunk.Key, chunk.Text, strings.Join(chunk.HeadingPath, "\x00"))
			chunks = append(chunks, chunk)
		}
	}
	return chunks, truncated
}

type unitChunkSpan struct {
	Text      string
	CharStart int
	CharEnd   int
}

func chunkUnitText(text string, maxRunes, overlapRunes int) []unitChunkSpan {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return []unitChunkSpan{{Text: text, CharEnd: len(runes)}}
	}
	spans := make([]unitChunkSpan, 0, len(runes)/maxRunes+1)
	for cursor := 0; cursor < len(runes); {
		end := min(cursor+maxRunes, len(runes))
		cut := end
		if end < len(runes) {
			floor := max(cursor+(maxRunes*3/4), cursor+1)
			for i := end - 1; i >= floor; i-- {
				if runes[i] == '\n' {
					cut = i + 1
					break
				}
			}
			if cut == end {
				for i := end - 1; i >= floor; i-- {
					if unicode.IsSpace(runes[i]) {
						cut = i + 1
						break
					}
				}
			}
		}
		spans = append(spans, unitChunkSpan{Text: string(runes[cursor:cut]), CharStart: cursor, CharEnd: cut})
		if cut == len(runes) {
			break
		}
		cursor += max((cut-cursor)-overlapRunes, 1)
	}
	return spans
}

func compactHeadingPath(path []string) []string {
	result := make([]string, 0, len(path))
	for _, part := range path {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func headingPathAt(marks []HeadingMark, offset int) []string {
	var result []string
	for _, mark := range marks {
		if mark.CharOffset > offset {
			break
		}
		result = mark.Path
	}
	return append([]string(nil), result...)
}

func checksumStrings(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hash, fmt.Sprintf("%d:", len(value)))
		_, _ = io.WriteString(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
