//go:build windows

package config

import "io/fs"

// sameConfigModePerm reports whether two observed config modes agree on the
// permission information Windows can actually report. Go derives permission
// bits on Windows from the read-only attribute only (0666 for writable files,
// 0444 for read-only ones); it can never observe the 0600 mode that config
// creation requests, so requiring exact equality would reject every newly
// created config. Compare the writable/read-only distinction instead — a
// concurrent read-only flip is still detected — while the owner-only DACL
// that actually protects the config is verified separately by
// validateOpenedConfigSecurity.
func sameConfigModePerm(left, right fs.FileMode) bool {
	writable := func(mode fs.FileMode) bool { return mode.Perm()&0o222 != 0 }
	return writable(left) == writable(right)
}
