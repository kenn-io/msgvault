package documentindex

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"go.kenn.io/docbank/document/ocr"
	"go.kenn.io/msgvault/internal/store"
)

// documentFailureDetail selects safe local diagnostics. Provider/normalization
// errors may contain response content, so their reason code is the diagnostic.
func documentFailureDetail(err error) string {
	if !errors.Is(err, errDocumentPreparation) {
		return ""
	}
	if errors.Is(err, errDocumentSizeBounds) {
		return errDocumentSizeBounds.Error()
	}
	if errors.Is(err, errDocumentSizeMismatch) {
		return errDocumentSizeMismatch.Error()
	}
	// Docbank owns content-safe descriptions of its parsers and staging errors.
	if preparation, ok := errors.AsType[*ocr.PreparationError](err); ok {
		return preparation.Error()
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
	if errors.Is(err, errDocumentAttachmentOpen) {
		return errDocumentAttachmentOpen.Error()
	}
	// Unknown local preparation errors can include source content or paths.
	return "invalid local document source"
}
