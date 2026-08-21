//go:build windows

package tui

import "os"

// markAttachmentUntrusted applies Windows Mark-of-the-Web through the NTFS
// Zone.Identifier alternate data stream before the platform handler sees the
// file. Failure is fatal to opening; the downloaded file remains available.
func markAttachmentUntrusted(path string) error {
	return os.WriteFile(path+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o600)
}
