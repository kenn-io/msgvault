//go:build !linux && !darwin && !windows

package peoplesweep

import "errors"

func validateCodexLaunchArtifact(string, CodexLaunchArtifact) error {
	return errors.New("Codex launch artifact is unsupported on this platform")
}
