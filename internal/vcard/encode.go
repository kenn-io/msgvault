package vcard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const defaultFoldBytes = 75

// EncodeOptions controls physical-line folding.
type EncodeOptions struct {
	FoldBytes int
}

// Encode writes a document using CRLF and 75-octet folding.
func Encode(w io.Writer, doc Document) error {
	return EncodeWithOptions(w, doc, EncodeOptions{FoldBytes: defaultFoldBytes})
}

// EncodeWithOptions writes a document using deterministic vCard syntax.
func EncodeWithOptions(w io.Writer, doc Document, opts EncodeOptions) error {
	if w == nil {
		return errors.New("vCard writer is nil")
	}
	if opts.FoldBytes == 0 {
		opts.FoldBytes = defaultFoldBytes
	}
	if opts.FoldBytes < 4 {
		return errors.New("fold limit must be at least 4 bytes")
	}

	for cardIndex, card := range doc.Cards {
		version, versionErr := card.Version()
		rfc6868 := versionErr == nil && version == Version40
		if err := writeFoldedLine(w, "BEGIN:VCARD", opts.FoldBytes); err != nil {
			return fmt.Errorf("encode card %d BEGIN: %w", cardIndex+1, err)
		}
		for propertyIndex, property := range card.Properties {
			line, err := renderProperty(property, rfc6868)
			if err != nil {
				return fmt.Errorf(
					"encode card %d property %d: %w",
					cardIndex+1,
					propertyIndex+1,
					err,
				)
			}
			if err := writeFoldedLine(w, line, opts.FoldBytes); err != nil {
				return fmt.Errorf(
					"encode card %d property %d: %w",
					cardIndex+1,
					propertyIndex+1,
					err,
				)
			}
		}
		if err := writeFoldedLine(w, "END:VCARD", opts.FoldBytes); err != nil {
			return fmt.Errorf("encode card %d END: %w", cardIndex+1, err)
		}
	}
	return nil
}

// Marshal encodes a document into vCard bytes.
func Marshal(doc Document) ([]byte, error) {
	var output bytes.Buffer
	if err := Encode(&output, doc); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func renderProperty(property Property, rfc6868 bool) (string, error) {
	if err := validateSyntaxToken("group", property.Group, true); err != nil {
		return "", err
	}
	name := property.OriginalName
	if name == "" {
		name = property.Name
	}
	if err := validateSyntaxToken("property name", name, false); err != nil {
		return "", err
	}
	if containsInjection(property.RawValue) {
		return "", errors.New("raw value contains CR, LF, or NUL")
	}

	var rendered strings.Builder
	if property.Group != "" {
		rendered.WriteString(property.Group)
		rendered.WriteByte('.')
	}
	rendered.WriteString(name)
	for _, parameter := range property.Parameters {
		value, err := renderParameter(parameter, rfc6868)
		if err != nil {
			return "", err
		}
		rendered.WriteByte(';')
		rendered.WriteString(value)
	}
	rendered.WriteByte(':')
	rendered.WriteString(property.RawValue)
	return rendered.String(), nil
}

func renderParameter(parameter Parameter, rfc6868 bool) (string, error) {
	if parameter.Bare {
		if parameter.OriginalName != "" || !strings.EqualFold(parameter.Name, parameterTypeName) {
			return "", errors.New("bare parameter must use TYPE without an original name")
		}
		if len(parameter.Values) == 0 {
			return "", errors.New("bare parameter must contain preserved raw values")
		}
		var rendered strings.Builder
		for i, value := range parameter.Values {
			if !value.RawValid {
				return "", errors.New("bare parameter must contain preserved raw values")
			}
			if len(parameter.Values) == 1 && value.Raw == "" && !value.Quoted {
				return "", errors.New("bare parameter value must not be empty unless quoted")
			}
			if !value.Quoted && strings.ContainsRune(value.Raw, '=') {
				return "", errors.New("bare unquoted parameter value contains '='")
			}
			if i > 0 {
				rendered.WriteByte(',')
			}
			encoded, err := renderParameterValue(value, rfc6868)
			if err != nil {
				return "", err
			}
			rendered.WriteString(encoded)
		}
		return rendered.String(), nil
	}

	name := parameter.OriginalName
	if name == "" {
		name = parameter.Name
	}
	if err := validateSyntaxToken("parameter name", name, false); err != nil {
		return "", err
	}
	var rendered strings.Builder
	rendered.WriteString(name)
	rendered.WriteByte('=')
	for i, value := range parameter.Values {
		if i > 0 {
			rendered.WriteByte(',')
		}
		encoded, err := renderParameterValue(value, rfc6868)
		if err != nil {
			return "", err
		}
		rendered.WriteString(encoded)
	}
	return rendered.String(), nil
}

func renderParameterValue(value ParameterValue, rfc6868 bool) (string, error) {
	raw := value.Raw
	quoted := value.Quoted
	if value.RawValid {
		if containsInjection(raw) {
			return "", errors.New("parameter value contains CR, LF, or NUL")
		}
		if strings.ContainsRune(raw, '"') {
			return "", errors.New("parameter value contains an unencoded quote")
		}
		if !quoted && strings.ContainsAny(raw, ":;,") {
			return "", errors.New("unquoted parameter value contains a delimiter")
		}
	} else {
		if strings.ContainsAny(value.Decoded, "\r\x00") || (!rfc6868 && strings.ContainsRune(value.Decoded, '\n')) {
			return "", errors.New("parameter value contains CR, LF, or NUL")
		}
		raw = value.Decoded
		if rfc6868 {
			raw = encodeRFC6868(raw)
		} else if strings.ContainsRune(raw, '"') {
			return "", errors.New("parameter value contains an unencoded quote")
		}
		quoted = quoted || strings.ContainsAny(raw, ":;,")
	}
	if quoted {
		return `"` + raw + `"`, nil
	}
	return raw, nil
}

func encodeRFC6868(value string) string {
	var encoded strings.Builder
	encoded.Grow(len(value))
	for _, r := range value {
		switch r {
		case '^':
			encoded.WriteString("^^")
		case '\n':
			encoded.WriteString("^n")
		case '"':
			encoded.WriteString("^'")
		default:
			encoded.WriteRune(r)
		}
	}
	return encoded.String()
}

func validateSyntaxToken(label, token string, allowEmpty bool) error {
	if token == "" && allowEmpty {
		return nil
	}
	if containsInjection(token) {
		return fmt.Errorf("%s contains CR, LF, or NUL", label)
	}
	if !validToken(token) {
		if token == "" {
			return fmt.Errorf("%s is empty", label)
		}
		return fmt.Errorf("invalid %s %q", label, token)
	}
	return nil
}

func containsInjection(value string) bool {
	return strings.ContainsAny(value, "\r\n\x00")
}

func writeFoldedLine(w io.Writer, line string, limit int) error {
	validUTF8 := utf8.ValidString(line)
	if !validUTF8 && !contentLineHasValidEncoding(line) {
		return errors.New("content line is not valid UTF-8")
	}
	quotedPrintableValueStart := -1
	if isQuotedPrintableContentLine(line) {
		colon, err := delimiterOutsideQuotes(line, ':')
		if err != nil {
			return err
		}
		quotedPrintableValueStart = colon + 1
	}
	remaining := line
	offset := 0
	first := true
	for {
		prefix := ""
		available := limit
		if !first {
			prefix = " "
			available--
		}

		take := len(remaining)
		if take > available {
			take = available
			if validUTF8 {
				for take > 0 && !utf8.RuneStart(remaining[take]) {
					take--
				}
			}
			if take == 0 {
				return fmt.Errorf("fold limit %d cannot fit the next UTF-8 code point", limit)
			}
			take = quotedPrintableSafeFold(
				line,
				offset,
				take,
				available,
				quotedPrintableValueStart,
			)
		}
		if err := writeFull(w, []byte(prefix+remaining[:take]+"\r\n")); err != nil {
			return err
		}
		remaining = remaining[take:]
		offset += take
		if len(remaining) == 0 {
			return nil
		}
		first = false
	}
}

func quotedPrintableSafeFold(line string, start, take, available, valueStart int) int {
	if valueStart < 0 {
		return take
	}
	cut := start + take
	if cut <= valueStart || cut >= len(line) {
		return take
	}

	escapeSearchStart := max(valueStart, cut-2)
	for escapeStart := escapeSearchStart; escapeStart < cut; escapeStart++ {
		if line[escapeStart] != '=' ||
			escapeStart+2 >= len(line) ||
			!isHexDigit(line[escapeStart+1]) ||
			!isHexDigit(line[escapeStart+2]) {
			continue
		}
		if escapeStart > start {
			return escapeStart - start
		}
		if escapeEnd := escapeStart + 3; escapeEnd-start <= available {
			return escapeEnd - start
		}
	}

	if line[cut-1] == '=' {
		if cut-1 > start {
			return take - 1
		}
		if take < available {
			return take + 1
		}
	}
	return take
}

func isHexDigit(value byte) bool {
	return value >= '0' && value <= '9' ||
		value >= 'A' && value <= 'F' ||
		value >= 'a' && value <= 'f'
}

func writeFull(w io.Writer, data []byte) error {
	written, err := w.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return nil
}
