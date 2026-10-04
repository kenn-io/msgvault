//go:build !unix

package peoplesweep

import "os"

const legacyCredentialOpenFlags = os.O_RDONLY
