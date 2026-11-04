//go:build !windows

package metawebhook

func transientSpoolReadError(error) bool { return false }
