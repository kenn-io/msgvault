//go:build darwin

package peoplesweep

import (
	"debug/macho"
	"errors"
	"runtime"
	"strings"
)

func validateCodexLaunchArtifact(path string, artifact CodexLaunchArtifact) error {
	if artifact != CodexLaunchArtifactNativeStandaloneV1 {
		return errors.New("unsupported Codex launch artifact")
	}
	file, err := macho.Open(path)
	if err != nil {
		return errors.New("read native Codex launch artifact")
	}
	defer func() { _ = file.Close() }()
	if file.Type != macho.TypeExec || file.Cpu != nativeCodexMachOCPU() {
		return errors.New("codex launch artifact is not a native executable")
	}
	libraries, err := file.ImportedLibraries()
	if err != nil {
		return errors.New("read codex launch artifact dependencies")
	}
	for _, library := range libraries {
		if !strings.HasPrefix(library, "/usr/lib/") && !strings.HasPrefix(library, "/System/Library/") {
			return errors.New("codex launch artifact requires a non-system library")
		}
	}
	return nil
}

func nativeCodexMachOCPU() macho.Cpu {
	switch runtime.GOARCH {
	case "amd64":
		return macho.CpuAmd64
	case "arm64":
		return macho.CpuArm64
	default:
		return 0
	}
}
