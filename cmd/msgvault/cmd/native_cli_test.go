package cmd

import "testing"

// Keep the native CLI cases in one shard so they share a single build.
func TestNativeCLIArtifacts(t *testing.T) {
	binary := buildNativeCLI(t)
	t.Run("MuesliHookBuiltArtifactDispatch", func(t *testing.T) {
		testMuesliHookBuiltArtifactDispatch(t, binary)
	})
	t.Run("DaemonSubprocessesPreserveEffectiveServeConfig", func(t *testing.T) {
		testDaemonSubprocessesPreserveEffectiveServeConfig(t, binary)
	})
	t.Run("DaemonCLIRunCannotUseServerRemoteDeleteConfigOrEnvironment", func(t *testing.T) {
		testDaemonCLIRunCannotUseServerRemoteDeleteConfigOrEnvironment(t, binary)
	})
	t.Run("PersonRenameHTTPRefreshesCachedPeople", func(t *testing.T) {
		testPersonRenameHTTPRefreshesCachedPeople(t, binary)
	})
}
