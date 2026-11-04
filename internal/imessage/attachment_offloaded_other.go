//go:build !darwin

package imessage

import "os"

func attachmentOffloaded(os.FileInfo) bool { return false }
