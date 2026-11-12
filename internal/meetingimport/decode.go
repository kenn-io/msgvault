package meetingimport

import (
	"io"

	"go.kenn.io/msgvault/internal/apiimport"
)

// DecodeRequest reads one strict meeting import request of at most maxBytes.
func DecodeRequest(r io.Reader, maxBytes int64) (Request, error) {
	return apiimport.Decode[Request](r, maxBytes)
}
