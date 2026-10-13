package mime

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"slices"

	"github.com/jhillyerd/enmime/v2"
)

// PartFingerprint describes one parsed MIME part without retaining its bytes.
type PartFingerprint struct {
	PartID                string
	HeaderHash            string
	RestorableHeaderHash  string
	ContentHash           string
	PreambleHash          string
	EpilogueHash          string
	HasAppleContentLength bool
}

// ParseWithRecoveryAndPartFingerprints also fingerprints the parsed MIME tree.
// Callers can compare copies outside Apple Mail placeholder positions without
// retaining the raw or decoded attachment bodies. Fingerprints are unavailable
// when parsing loses content or raw framing disagrees with the parsed tree.
func ParseWithRecoveryAndPartFingerprints(raw []byte, fallbackSubject string) (*Message, []PartFingerprint, error) {
	var fingerprints []PartFingerprint
	msg, err := parseWithRecovery(raw, fallbackSubject, func(root *enmime.Part) {
		parts := root.DepthMatchAll(func(*enmime.Part) bool { return true })
		for _, part := range parts {
			for _, problem := range part.Errors {
				if problem.Severe {
					return
				}
			}
		}
		rawMessage, rawErr := mail.ReadMessage(bytes.NewReader(raw))
		if rawErr != nil {
			return
		}
		rawHashes := make(map[string]rawPartHashes, len(parts))
		if !fingerprintRawParts(rawMessage.Body, root, rawHashes) {
			return
		}
		fingerprints = make([]PartFingerprint, 0, len(parts))
		for _, part := range parts {
			_, placeholder := part.Header["X-Apple-Content-Length"]
			fingerprints = append(fingerprints, PartFingerprint{
				PartID: part.PartID, HeaderHash: fingerprintHeaders(part.Header, false),
				RestorableHeaderHash:  fingerprintHeaders(part.Header, true),
				ContentHash:           rawHashes[part.PartID].content,
				PreambleHash:          rawHashes[part.PartID].preamble,
				EpilogueHash:          partHash(part.Epilogue),
				HasAppleContentLength: placeholder,
			})
		}
	})
	return msg, fingerprints, err
}

type rawPartHashes struct {
	content, preamble string
}

// fingerprintRawParts accounts for bytes enmime can discard or normalize.
// NextRawPart preserves transfer encoding; the parsed tree supplies boundaries
// and the headers must agree before the raw framing is trusted.
func fingerprintRawParts(body io.Reader, part *enmime.Part, hashes map[string]rawPartHashes) bool {
	if part.Boundary == "" {
		if part.FirstChild != nil {
			return false
		}
		h := sha256.New()
		if _, err := io.Copy(h, body); err != nil {
			return false
		}
		hashes[part.PartID] = rawPartHashes{content: hex.EncodeToString(h.Sum(nil))}
		return true
	}

	reader := bufio.NewReader(body)
	opening, closing := "--"+part.Boundary, "--"+part.Boundary+"--"
	preamble := sha256.New()
	var delimiter []byte
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := string(bytes.TrimRight(line, "\r\n \t"))
		if trimmed == opening || trimmed == closing {
			delimiter = line
			break
		}
		if err != nil {
			return false
		}
		_, _ = preamble.Write(line)
	}
	hashes[part.PartID] = rawPartHashes{preamble: hex.EncodeToString(preamble.Sum(nil))}
	readerParts := multipart.NewReader(io.MultiReader(bytes.NewReader(delimiter), reader), part.Boundary)
	for child := part.FirstChild; child != nil; child = child.NextSibling {
		rawChild, err := readerParts.NextRawPart()
		if err != nil || fingerprintHeaders(rawChild.Header, false) != fingerprintHeaders(child.Header, false) {
			return false
		}
		if !fingerprintRawParts(rawChild, child, hashes) {
			return false
		}
	}
	_, err := readerParts.NextRawPart()
	return err == io.EOF
}

func fingerprintHeaders(headers textproto.MIMEHeader, restorable bool) string {
	var encoded []byte
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		if restorable && (name == "X-Apple-Content-Length" || name == "Content-Transfer-Encoding") {
			continue
		}
		values := headers[name]
		// Length prefixes preserve all bytes, including invalid UTF-8, and
		// distinguish duplicate headers from values containing delimiters.
		encoded = fmt.Appendf(encoded, "%d:%s%d:", len(name), name, len(values))
		for _, value := range values {
			encoded = fmt.Appendf(encoded, "%d:", len(value))
			encoded = append(encoded, value...)
		}
	}
	return partHash(encoded)
}

func partHash(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}
