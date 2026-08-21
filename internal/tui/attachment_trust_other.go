//go:build !darwin && !windows

package tui

// Linux and the supported BSDs do not expose a portable OS download-origin
// marker. The strict passive-type allowlist remains the opening boundary.
func markAttachmentUntrusted(string) error { return nil }
