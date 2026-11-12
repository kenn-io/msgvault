//go:build !windows

package providercredentials

func restorePreflight(string, map[string]struct{}) error { return nil }

func secureRestoredFile(string) error { return nil }
