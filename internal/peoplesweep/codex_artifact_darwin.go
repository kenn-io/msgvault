//go:build darwin

package peoplesweep

import (
	"bytes"
	"debug/macho"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// Mach-O load commands that name code dyld loads, or change where it looks.
// debug/macho decodes only LC_LOAD_DYLIB into ImportedLibraries, so the rest
// are read from the raw commands.
const (
	machoLoadDylinker      = 0xe
	machoLoadDylib         = 0xc
	machoLoadWeakDylib     = 0x80000018
	machoReexportDylib     = 0x8000001f
	machoLazyLoadDylib     = 0x20
	machoLoadUpwardDylib   = 0x80000023
	machoDyldEnvironment   = 0x27
	machoDylibNameOffset   = 8
	machoLoadCommandHeader = 12
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
	for _, load := range file.Loads {
		raw := load.Raw()
		if len(raw) < machoLoadCommandHeader {
			return errors.New("read codex launch artifact dependencies")
		}
		switch file.ByteOrder.Uint32(raw) {
		case machoLoadDylinker:
			if name, ok := machoLoadCommandName(raw, file); !ok || name != "/usr/lib/dyld" {
				return errors.New("codex launch artifact requires a non-system loader")
			}
		case machoLoadDylib, machoLoadWeakDylib, machoReexportDylib, machoLazyLoadDylib, machoLoadUpwardDylib:
			name, ok := machoLoadCommandName(raw, file)
			if !ok {
				return errors.New("read codex launch artifact dependencies")
			}
			if !systemMachOLibrary(name) {
				return errors.New("codex launch artifact requires a non-system library")
			}
		case machoDyldEnvironment:
			return errors.New("codex launch artifact sets a loader environment")
		}
	}
	return nil
}

// systemMachOLibrary admits only a clean absolute path under the system
// library roots, so "/usr/lib/../../tmp/x.dylib" does not pass as system code.
func systemMachOLibrary(name string) bool {
	return filepath.Clean(name) == name &&
		(strings.HasPrefix(name, "/usr/lib/") || strings.HasPrefix(name, "/System/Library/"))
}

func machoLoadCommandName(raw []byte, file *macho.File) (string, bool) {
	offset := int(file.ByteOrder.Uint32(raw[machoDylibNameOffset:]))
	if offset < machoLoadCommandHeader || offset >= len(raw) {
		return "", false
	}
	name, _, found := bytes.Cut(raw[offset:], []byte{0})
	return string(name), found && len(name) > 0
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
