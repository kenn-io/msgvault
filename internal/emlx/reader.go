// Package emlx parses Apple Mail .emlx files and discovers mailbox directories.
//
// The .emlx format stores one message per file:
//   - Line 1: decimal byte count of the raw MIME content
//   - Next N bytes: raw RFC 5322 MIME message
//   - Remainder (optional): XML plist with Apple Mail metadata
package emlx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Message represents a parsed .emlx file.
type Message struct {
	// Raw is the RFC 5322 MIME content.
	Raw []byte

	// SourceHash is the SHA-256 of the original MIME bytes, before restoring
	// attachments. It remains stable when Apple Mail downloads attachments.
	SourceHash string

	// PlistDate is the date-sent value from the plist metadata.
	// Zero if the plist is missing or the field is absent.
	PlistDate time.Time

	// Flags is the Apple Mail flags integer from the plist.
	Flags int

	// OrigMailbox is the original-mailbox value from the plist.
	OrigMailbox string

	// RestoredAttachments is the number of attachment parts whose
	// placeholder body was replaced with content from Apple Mail's
	// sibling Attachments/ directory (see ParseFile).
	RestoredAttachments int

	// RestorationError reports unreadable cached attachments. Raw still
	// contains the message, with placeholders for parts that could not be read.
	RestorationError error
}

// Parse parses an .emlx file from its raw bytes.
func Parse(data []byte) (*Message, error) {
	if len(data) == 0 {
		return nil, errors.New("emlx: empty file")
	}

	// Line 1: byte count (terminated by \n).
	newline := bytes.IndexByte(data, '\n')
	if newline < 0 {
		return nil, errors.New("emlx: no newline after byte count")
	}
	countStr := strings.TrimSpace(string(data[:newline]))
	// bitSize 0 bounds the parsed count to the platform int, so the int()
	// conversion below can never truncate; absurd counts fail here with a
	// range error.
	byteCount, err := strconv.ParseInt(countStr, 10, 0)
	if err != nil {
		return nil, fmt.Errorf("emlx: invalid byte count %q: %w", countStr, err)
	}
	if byteCount < 0 {
		return nil, fmt.Errorf("emlx: negative byte count %d", byteCount)
	}

	mimeStart := newline + 1
	available := int64(len(data) - mimeStart)
	if byteCount > available {
		return nil, fmt.Errorf(
			"emlx: byte count %d exceeds file size (available: %d)",
			byteCount, available,
		)
	}
	mimeEnd := mimeStart + int(byteCount)

	msg := &Message{
		Raw: data[mimeStart:mimeEnd],
	}
	sum := sha256.Sum256(msg.Raw)
	msg.SourceHash = hex.EncodeToString(sum[:])

	// Parse optional plist metadata (best-effort).
	if mimeEnd < len(data) {
		plistData := data[mimeEnd:]
		parsePlist(plistData, msg)
	}

	return msg, nil
}

// ParseFile reads and parses an .emlx file from disk.
//
// For a Messages/<num>.partial.emlx file, Apple Mail keeps attachment bytes
// out of the MIME payload and stores them in a sibling Attachments/<num>/
// directory instead, leaving an X-Apple-Content-Length placeholder in the
// part header. ParseFile restores top-level attachment parts into Raw as
// base64. Nested attachments keep their placeholders.
// Attachments are only restored while the message, with the
// restored parts base64-encoded, stays within maxBytes; a part that would
// exceed the remaining budget keeps its placeholder.
func ParseFile(path string, maxBytes int64) (*Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("emlx: read %q: %w", path, err)
	}
	msg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	msg.Raw, msg.RestoredAttachments, msg.RestorationError = RestoreAttachments(msg.Raw, path, maxBytes)
	return msg, nil
}

// parsePlist extracts metadata from the Apple Mail XML plist.
// Failures are silently ignored (best-effort).
func parsePlist(data []byte, msg *Message) {
	// Find the plist XML start.
	start := bytes.Index(data, []byte("<?xml"))
	if start < 0 {
		start = bytes.Index(data, []byte("<plist"))
	}
	if start < 0 {
		return
	}
	data = data[start:]

	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = false

	// Walk tokens looking for <dict> key/value pairs.
	var currentKey string
	inDict := false

	for {
		tok, err := decoder.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "dict":
				inDict = true
			case "key":
				if inDict {
					var val string
					if err := decoder.DecodeElement(&val, &t); err != nil {
						return
					}
					currentKey = val
				}
			case "real":
				if inDict && currentKey == "date-sent" {
					var val string
					if err := decoder.DecodeElement(&val, &t); err != nil {
						return
					}
					if f, err := strconv.ParseFloat(val, 64); err == nil {
						// Apple's epoch is 2001-01-01 00:00:00 UTC.
						appleEpoch := time.Date(
							2001, 1, 1, 0, 0, 0, 0, time.UTC,
						)
						msg.PlistDate = appleEpoch.Add(
							time.Duration(f * float64(time.Second)),
						)
					}
					currentKey = ""
				}
			case "integer":
				if inDict {
					var val string
					if err := decoder.DecodeElement(&val, &t); err != nil {
						return
					}
					switch currentKey {
					case "flags":
						if n, err := strconv.Atoi(val); err == nil {
							msg.Flags = n
						}
					case "date-sent":
						// Some plists use integer instead of real.
						if n, err := strconv.ParseInt(val, 10, 64); err == nil {
							appleEpoch := time.Date(
								2001, 1, 1, 0, 0, 0, 0, time.UTC,
							)
							msg.PlistDate = appleEpoch.Add(
								time.Duration(n) * time.Second,
							)
						}
					}
					currentKey = ""
				}
			case "string":
				if inDict && currentKey == "original-mailbox" {
					var val string
					if err := decoder.DecodeElement(&val, &t); err != nil {
						return
					}
					msg.OrigMailbox = val
					currentKey = ""
				}
			}
		case xml.EndElement:
			if t.Name.Local == "dict" {
				inDict = false
			}
		}
	}
}
