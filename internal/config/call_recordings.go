package config

import (
	"fmt"
	"math"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// defaultCallRecordingMaxBytes caps each call recording at 1 GiB, the largest
// file msgvault's Docbank audio route delivers, unless max_media_mb is set.
const defaultCallRecordingMaxBytes = 1 << 30

// callRecordingMediaPolicy is the media policy shared by call-recording
// sources: media toggles downloads and maxMediaMB overrides the default cap.
func callRecordingMediaPolicy(media *bool, maxMediaMB int) attachmentpolicy.Policy {
	return resolveMediaPolicy(media, "", 0, maxMediaMB, defaultCallRecordingMaxBytes, MediaAccountConfig{}, false)
}

// validateCallRecordingCap rejects a max_media_mb that is negative or too
// large to express in bytes.
func validateCallRecordingCap(table, identifier string, maxMediaMB int) error {
	if maxMediaMB < 0 || int64(maxMediaMB) > math.MaxInt64>>20 {
		return fmt.Errorf("[[%s]] identifier %q: max_media_mb must be zero or a positive representable MiB limit", table, identifier)
	}
	return nil
}
