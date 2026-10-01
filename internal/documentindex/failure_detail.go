package documentindex

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/msgvault/internal/store"
)

// documentFailureDetail selects safe local diagnostics. Provider/normalization
// errors may contain response content, so their reason code is the diagnostic.
func documentFailureDetail(err error) string {
	if !errors.Is(err, errDocumentPreparation) {
		return ""
	}
	text := err.Error()
	// Spool sentinels can wrap low-level I/O details; keep their stable diagnosis.
	if errors.Is(err, mistral.ErrSpoolCapacity) {
		return "document spool capacity unavailable: quota or free-space reserve exhausted"
	}
	if errors.Is(err, mistral.ErrSpoolUnavailable) {
		return "document spool temporarily unavailable"
	}
	// Local unit parsers can echo XML names, ZIP entries and relationship targets.
	if strings.Contains(text, "count local Mistral OCR units:") {
		return "invalid document structure while counting local units"
	}
	if strings.Contains(text, "verify generated PDF:") {
		return "invalid generated PDF structure"
	}
	// PDF object parsing can include dictionary keys and tokens from the source.
	if strings.Contains(text, "PDF object") || strings.Contains(text, "PDF dictionary") ||
		strings.Contains(text, "PDF embedded file") || strings.Contains(text, "decode PDF stream:") {
		return "invalid PDF structure"
	}
	if strings.Contains(text, "PDF end marker is missing or not final") {
		return "PDF end marker is missing or not final"
	}
	if pathErr, ok := errors.AsType[*os.PathError](err); ok {
		if errors.Is(pathErr.Err, os.ErrNotExist) {
			return "local document I/O: file does not exist"
		}
		if errors.Is(pathErr.Err, os.ErrPermission) {
			return "local document I/O: permission denied"
		}
		if errno, ok := errors.AsType[syscall.Errno](pathErr.Err); ok {
			return store.CleanDocumentFailureDetail(fmt.Sprintf("local document I/O (%s): %s", pathErr.Op, errno))
		}
		return "local document I/O failed"
	}
	// Openers/readers are application dependencies, not a content-safe error
	// contract. Keep fixed bounds and integrity diagnostics; mask arbitrary I/O.
	if strings.Contains(text, "open document attachment:") {
		return "open document attachment failed"
	}
	if strings.Contains(text, "copy Mistral OCR spool:") || strings.Contains(text, "close Mistral OCR source:") || strings.Contains(text, "verify Mistral OCR source length:") {
		return "read document attachment failed"
	}
	// Unknown local preparation errors can include source content or paths.
	return "invalid local document source"
}
