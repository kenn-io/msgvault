//go:build windows

package peoplesweep

import (
	"debug/pe"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

func validateCodexLaunchArtifact(path string, artifact CodexLaunchArtifact) error {
	if artifact != CodexLaunchArtifactNativeStandaloneV1 ||
		!strings.EqualFold(filepath.Ext(path), ".exe") {
		return errors.New("unsupported Codex launch artifact")
	}
	file, err := pe.Open(path)
	if err != nil {
		return errors.New("read native Codex launch artifact")
	}
	defer func() { _ = file.Close() }()
	if file.Machine != nativeCodexPEMachine() || file.OptionalHeader == nil ||
		file.Characteristics&pe.IMAGE_FILE_DLL != 0 {
		return errors.New("codex launch artifact is not a native executable")
	}
	libraries, err := file.ImportedLibraries()
	if err != nil || len(libraries) != 0 {
		return errors.New("codex launch artifact requires dynamic libraries")
	}
	return nil
}

func nativeCodexPEMachine() uint16 {
	switch runtime.GOARCH {
	case "386":
		return pe.IMAGE_FILE_MACHINE_I386
	case "amd64":
		return pe.IMAGE_FILE_MACHINE_AMD64
	case "arm":
		return pe.IMAGE_FILE_MACHINE_ARM
	case "arm64":
		return pe.IMAGE_FILE_MACHINE_ARM64
	default:
		return 0
	}
}
