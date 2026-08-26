//go:build linux

package peoplesweep

import (
	"debug/elf"
	"errors"
	"runtime"
)

func validateCodexLaunchArtifact(path string, artifact CodexLaunchArtifact) error {
	if artifact != CodexLaunchArtifactNativeStandaloneV1 {
		return errors.New("unsupported Codex launch artifact")
	}
	file, err := elf.Open(path)
	if err != nil {
		return errors.New("read native Codex launch artifact")
	}
	defer func() { _ = file.Close() }()
	if file.Type != elf.ET_EXEC || file.Machine != nativeCodexELFMachine() {
		return errors.New("codex launch artifact is not a native executable")
	}
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			return errors.New("codex launch artifact requires an interpreter")
		}
	}
	libraries, err := file.ImportedLibraries()
	if err != nil || len(libraries) != 0 {
		return errors.New("codex launch artifact requires dynamic libraries")
	}
	return nil
}

func nativeCodexELFMachine() elf.Machine {
	switch runtime.GOARCH {
	case "386":
		return elf.EM_386
	case "amd64":
		return elf.EM_X86_64
	case "arm":
		return elf.EM_ARM
	case "arm64":
		return elf.EM_AARCH64
	case "loong64":
		return elf.EM_LOONGARCH
	case "ppc64", "ppc64le":
		return elf.EM_PPC64
	case "riscv64":
		return elf.EM_RISCV
	case "s390x":
		return elf.EM_S390
	default:
		return elf.EM_NONE
	}
}
