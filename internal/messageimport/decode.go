package messageimport

import (
	"io"

	"go.kenn.io/msgvault/internal/apiimport"
)

// DecodeRequest reads one strict message import request of at most maxBytes.
// Unknown members are rejected everywhere except inside message.metadata,
// whose numbers keep their exact text.
func DecodeRequest(r io.Reader, maxBytes int64) (Request, error) {
	return apiimport.Decode[Request](r, maxBytes)
}
