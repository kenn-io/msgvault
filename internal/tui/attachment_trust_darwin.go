//go:build darwin

package tui

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// markAttachmentUntrusted applies the same quarantine attribute Finder and
// browsers use so Gatekeeper participates when the default application opens
// a downloaded attachment.
func markAttachmentUntrusted(path string) error {
	value := fmt.Sprintf("0081;%x;msgvault;", time.Now().Unix())
	if err := unix.Setxattr(path, "com.apple.quarantine", []byte(value), 0); err != nil {
		return fmt.Errorf("set quarantine attribute: %w", err)
	}
	return nil
}
