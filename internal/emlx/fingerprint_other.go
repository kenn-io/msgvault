//go:build !linux && !darwin

package emlx

import "os"

func fileIdentity(os.FileInfo) (string, bool) { return "", false }
