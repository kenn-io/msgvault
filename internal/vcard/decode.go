package vcard

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxPhysicalLineBytes = 16 << 20
	DefaultMaxLogicalLineBytes  = 16 << 20
	DefaultMaxCards             = 100_000
)

type sourceLine struct {
	number int
	text   string
}

// Decode parses vCard 2.1, 3.0, and 4.0 syntax with default bounds.
func Decode(r io.Reader) (Document, error) {
	return DecodeWithOptions(r, DecodeOptions{})
}

// DecodeWithOptions parses ordered vCard syntax with explicit bounds.
func DecodeWithOptions(r io.Reader, opts DecodeOptions) (Document, error) {
	opts, err := normalizeDecodeOptions(opts)
	if err != nil {
		return Document{}, err
	}

	decoder := documentDecoder{opts: opts}
	unfolder := logicalLineUnfolder{
		limit:   opts.MaxLogicalLineBytes,
		consume: decoder.consumeLogicalLine,
	}
	if err := readPhysicalLines(r, opts.MaxPhysicalLineBytes, unfolder.consumePhysicalLine); err != nil {
		return Document{}, err
	}
	if err := unfolder.finish(); err != nil {
		return Document{}, err
	}
	if err := decoder.finish(); err != nil {
		return Document{}, err
	}
	return decoder.document, nil
}

func normalizeDecodeOptions(opts DecodeOptions) (DecodeOptions, error) {
	if opts.MaxPhysicalLineBytes == 0 {
		opts.MaxPhysicalLineBytes = DefaultMaxPhysicalLineBytes
	}
	if opts.MaxLogicalLineBytes == 0 {
		opts.MaxLogicalLineBytes = DefaultMaxLogicalLineBytes
	}
	if opts.MaxCards == 0 {
		opts.MaxCards = DefaultMaxCards
	}
	if opts.MaxPhysicalLineBytes < 1 {
		return DecodeOptions{}, errors.New("maximum physical line bytes must be positive")
	}
	if opts.MaxLogicalLineBytes < 1 {
		return DecodeOptions{}, errors.New("maximum logical line bytes must be positive")
	}
	if opts.MaxCards < 1 {
		return DecodeOptions{}, errors.New("maximum cards must be positive")
	}
	return opts, nil
}

func readPhysicalLines(
	r io.Reader,
	limit int,
	consume func(number int, line []byte) error,
) error {
	if r == nil {
		return errors.New("vCard reader is nil")
	}
	reader := bufio.NewReader(r)
	var line []byte
	lineNumber := 1
	for {
		fragment, prefix, err := reader.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return &ParseError{PhysicalLine: lineNumber, Err: fmt.Errorf("read input: %w", err)}
		}
		if len(line)+len(fragment) > limit {
			return &ParseError{
				PhysicalLine: lineNumber,
				Err:          fmt.Errorf("physical line %d exceeds %d bytes", lineNumber, limit),
			}
		}
		if prefix {
			line = append(line, fragment...)
			continue
		}

		physical := fragment
		if len(line) > 0 {
			line = append(line, fragment...)
			physical = line
		}
		if bytes.IndexByte(physical, '\r') >= 0 {
			return &ParseError{PhysicalLine: lineNumber, Err: errors.New("bare CR is not allowed")}
		}
		if lineNumber == 1 {
			physical = bytes.TrimPrefix(physical, []byte{0xef, 0xbb, 0xbf})
		}
		if err := consume(lineNumber, physical); err != nil {
			return err
		}
		line = line[:0]
		lineNumber++
	}
	return nil
}

type logicalLineUnfolder struct {
	limit           int
	consume         func(sourceLine) error
	buffer          []byte
	physicalLine    int
	hasLine         bool
	headerScanned   int
	headerQuoted    bool
	headerComplete  bool
	quotedPrintable bool
}

func (u *logicalLineUnfolder) consumePhysicalLine(number int, line []byte) error {
	if !u.hasLine {
		return u.startLine(number, line)
	}

	switch {
	case u.quotedPrintable && len(u.buffer) > 0 && u.buffer[len(u.buffer)-1] == '=':
		u.buffer = u.buffer[:len(u.buffer)-1]
		if startsFoldBytes(line) {
			line = line[1:]
		}
		return u.appendContinuation(number, line)
	case startsFoldBytes(line):
		return u.appendContinuation(number, line[1:])
	default:
		if err := u.flush(); err != nil {
			return err
		}
		return u.startLine(number, line)
	}
}

func (u *logicalLineUnfolder) startLine(number int, line []byte) error {
	if startsFoldBytes(line) {
		return &ParseError{
			PhysicalLine: number,
			Err:          errors.New("folded continuation has no previous content line"),
		}
	}
	if len(line) > u.limit {
		return &ParseError{
			PhysicalLine: number,
			Err:          fmt.Errorf("logical content line exceeds %d bytes", u.limit),
		}
	}
	u.buffer = append(u.buffer[:0], line...)
	u.physicalLine = number
	u.hasLine = true
	u.headerScanned = 0
	u.headerQuoted = false
	u.headerComplete = false
	u.quotedPrintable = false
	u.scanHeader()
	return nil
}

func (u *logicalLineUnfolder) appendContinuation(number int, continuation []byte) error {
	if len(u.buffer)+len(continuation) > u.limit {
		return &ParseError{
			PhysicalLine: number,
			Err:          fmt.Errorf("logical content line exceeds %d bytes", u.limit),
		}
	}
	u.buffer = append(u.buffer, continuation...)
	u.scanHeader()
	return nil
}

func (u *logicalLineUnfolder) scanHeader() {
	if u.headerComplete {
		return
	}
	for i := u.headerScanned; i < len(u.buffer); i++ {
		switch u.buffer[i] {
		case '"':
			u.headerQuoted = !u.headerQuoted
		case ':':
			if u.headerQuoted {
				continue
			}
			u.headerComplete = true
			u.headerScanned = i + 1
			u.quotedPrintable = isQuotedPrintableHeader(string(u.buffer[:i]))
			return
		}
	}
	u.headerScanned = len(u.buffer)
}

func (u *logicalLineUnfolder) flush() error {
	if !u.hasLine {
		return nil
	}
	line := sourceLine{number: u.physicalLine, text: string(u.buffer)}
	if !contentLineHasValidEncoding(line.text) {
		return &ParseError{
			PhysicalLine: line.number,
			Err:          errors.New("content line is not valid UTF-8"),
		}
	}
	u.hasLine = false
	return u.consume(line)
}

func (u *logicalLineUnfolder) finish() error {
	return u.flush()
}

func contentLineHasValidEncoding(line string) bool {
	if utf8.ValidString(line) {
		return true
	}
	colon, err := delimiterOutsideQuotes(line, ':')
	return err == nil &&
		colon >= 0 &&
		utf8.ValidString(line[:colon]) &&
		contentLineDeclaresCharset(line[:colon])
}

func contentLineDeclaresCharset(header string) bool {
	parts, err := splitOutsideQuotesCapped(header, ';', maxParametersPerProperty+1)
	if err != nil {
		return false
	}
	for _, part := range parts[1:] {
		name, _, found := strings.Cut(part, "=")
		if found && strings.EqualFold(strings.TrimSpace(name), "CHARSET") {
			return true
		}
	}
	return false
}

func startsFoldBytes(line []byte) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

func isQuotedPrintableContentLine(line string) bool {
	colon, err := delimiterOutsideQuotes(line, ':')
	if err != nil || colon < 0 {
		return false
	}
	return isQuotedPrintableHeader(line[:colon])
}

func isQuotedPrintableHeader(header string) bool {
	parts, err := splitOutsideQuotesCapped(
		header,
		';',
		maxParametersPerProperty+1,
	)
	if err != nil {
		return false
	}
	for _, part := range parts[1:] {
		name, value, hasValue := strings.Cut(part, "=")
		if hasValue &&
			strings.EqualFold(name, "ENCODING") &&
			strings.EqualFold(strings.Trim(value, `"`), "QUOTED-PRINTABLE") {
			return true
		}
		if !hasValue && strings.EqualFold(part, "QUOTED-PRINTABLE") {
			return true
		}
	}
	return false
}

type documentDecoder struct {
	opts             DecodeOptions
	document         Document
	current          *Card
	lastPhysicalLine int
}

func (d *documentDecoder) consumeLogicalLine(line sourceLine) error {
	d.lastPhysicalLine = line.number
	if line.text == "" {
		return nil
	}
	property, err := parseContentLine(line.text)
	if err != nil {
		return parseError(line.number, len(d.document.Cards)+1, err)
	}
	framing := property.Group == "" && len(property.Parameters) == 0
	switch {
	case framing && property.Name == "BEGIN" && strings.EqualFold(property.RawValue, "VCARD"):
		if d.current != nil {
			return parseError(
				line.number,
				len(d.document.Cards)+1,
				errors.New("nested BEGIN:VCARD"),
			)
		}
		if len(d.document.Cards) >= d.opts.MaxCards {
			return parseError(
				line.number,
				len(d.document.Cards)+1,
				fmt.Errorf("card count exceeds %d", d.opts.MaxCards),
			)
		}
		d.current = &Card{}
	case framing && property.Name == "END" && strings.EqualFold(property.RawValue, "VCARD"):
		if d.current == nil {
			return parseError(line.number, 0, errors.New("stray END:VCARD"))
		}
		decodeParameterValues(d.current)
		d.document.Cards = append(d.document.Cards, *d.current)
		d.current = nil
	default:
		if d.current == nil {
			return parseError(line.number, 0, errors.New("content outside VCARD"))
		}
		if property.Name == "VERSION" &&
			strings.TrimSpace(property.RawValue) == string(Version21) &&
			d.opts.DisallowV21 {
			return parseError(
				line.number,
				len(d.document.Cards)+1,
				errors.New("vCard 2.1 is disabled"),
			)
		}
		d.current.Properties = append(d.current.Properties, property)
	}
	return nil
}

func decodeParameterValues(card *Card) {
	version, err := card.Version()
	if err == nil && version == Version40 {
		return
	}
	for propertyIndex := range card.Properties {
		for parameterIndex := range card.Properties[propertyIndex].Parameters {
			for valueIndex := range card.Properties[propertyIndex].Parameters[parameterIndex].Values {
				value := &card.Properties[propertyIndex].Parameters[parameterIndex].Values[valueIndex]
				value.Decoded = value.Raw
			}
		}
	}
}

func (d *documentDecoder) finish() error {
	if d.current != nil {
		return parseError(
			d.lastPhysicalLine,
			len(d.document.Cards)+1,
			errors.New("missing END:VCARD"),
		)
	}
	return nil
}

func parseError(physicalLine, cardIndex int, err error) error {
	return &ParseError{PhysicalLine: physicalLine, CardIndex: cardIndex, Err: err}
}
