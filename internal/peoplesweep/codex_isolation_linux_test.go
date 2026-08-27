//go:build linux

package peoplesweep

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodexRegisteredExecutableRejectsAdjacentDynamicDependency catches an
// otherwise-native binary loading unverified code from beside its snapshot.
func TestCodexRegisteredExecutableRejectsAdjacentDynamicDependency(t *testing.T) {
	must := require.New(t)
	compiler, err := exec.LookPath("cc")
	must.NoError(err, "the tagged SQLite build already requires a C compiler")
	root := t.TempDir()
	marker := filepath.Join(root, "adjacent-dependency-ran")
	dependencySource := filepath.Join(root, "dependency.c")
	launcherSource := filepath.Join(root, "launcher.c")
	dependency := filepath.Join(root, "libadjacent.so")
	executable := filepath.Join(root, "codex-adjacent")
	must.NoError(os.WriteFile(dependencySource, []byte(
		`const char *codex_version(void) { return "codex-cli 0.149.0"; }`), 0o600))
	must.NoError(os.WriteFile(launcherSource, []byte(
		"#include <stdio.h>\n"+
			"const char *codex_version(void);\n"+
			"int main(void) { FILE *f = fopen("+strconv.Quote(marker)+", \"w\"); "+
			"if (f) { fputs(\"ran\", f); fclose(f); } puts(codex_version()); return 0; }\n",
	), 0o600))
	compileCodexNativeFixture(t, compiler, "-shared", "-fPIC", dependencySource, "-o", dependency)
	compileCodexNativeFixture(
		t, compiler, launcherSource, "-L"+root, "-ladjacent", "-Wl,-rpath,$ORIGIN", "-o", executable,
	)
	contents, err := os.ReadFile(executable)
	must.NoError(err)

	attestation, err := verifyReleasedCodexIsolation(
		t.Context(), executable, CodexExecutionBoundaryV1, codexIsolationFixtureRegistry(contents),
	)
	must.ErrorIs(err, ErrCodexIsolationUnreleased)
	assert.Empty(t, attestation)
	assert.NoFileExists(t, marker)
}

func compileCodexNativeFixture(t *testing.T, compiler string, args ...string) {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), compiler, args...).CombinedOutput()
	require.NoError(t, err, string(output))
}
