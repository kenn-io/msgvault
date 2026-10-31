package cmd

import (
	"bytes"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/cligrammar"
	"go.kenn.io/msgvault/internal/store"
)

func TestCLIGrammarRealTree(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	requirements := require.New(t)
	assertions := assert.New(t)
	// Completion callbacks are keyed by Flag pointer in Cobra, not by name.
	original := directCommand(rootCmd, "draft-reply")
	requirements.NoError(original.RegisterFlagCompletionFunc("body", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"synthetic-completion"}, cobra.ShellCompDirectiveNoFileComp
	}))
	originals := make(map[string]*cobra.Command, len(cligrammar.Moves()))
	for _, move := range cligrammar.Moves() {
		originals[move.Legacy] = directCommand(rootCmd, move.Legacy)
		requirements.NotNil(originals[move.Legacy], move.Legacy)
	}
	ensureHelpLayout(rootCmd)
	ensureHelpLayout(rootCmd)
	ensureSilenceUsageWrapped(rootCmd)
	for _, move := range cligrammar.Moves() {
		t.Run(move.Legacy, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			old := originals[move.Legacy]
			_, _, err := rootCmd.Find([]string{move.Legacy})
			require.Error(err, "old CLI path must not be registered")
			canonical, rest, err := rootCmd.Find(strings.Fields(move.Canonical))
			require.NoError(err)
			require.Empty(rest)
			assert.NotSame(old, canonical)
			assert.False(canonical.Hidden)
			assert.Empty(canonical.Deprecated)
			for key, value := range old.Annotations {
				assert.Equal(value, canonical.Annotations[key], key)
			}
			wire, err := daemonCLIArgsFromCobra(canonical, []string{"42"})
			require.NoError(err)
			assert.Equal([]string{move.Legacy, "42"}, wire)
			assert.Equal(api.CLIRunCommandAllowed(wire), api.CLIRunCommandAllowed(append(strings.Fields(move.Canonical), "42")))
			executed, args, err := rootCmd.Find(cligrammar.Canonical(wire))
			require.NoError(err)
			assert.Same(canonical, executed)
			assert.Equal([]string{"42"}, args)
			old.LocalFlags().VisitAll(func(flag *pflag.Flag) {
				cloned := canonical.Flags().Lookup(flag.Name)
				require.NotNil(cloned)
				assert.NotSame(flag, cloned)
				assert.Same(flag.Value, cloned.Value)
				assert.Equal(flag.DefValue, cloned.DefValue)
				assert.Equal(flag.Annotations, cloned.Annotations)
				assert.Equal(flag.Value.Type(), cloned.Value.Type())
			})
		})
	}
	for _, path := range []string{"embeddings build", "embeddings status", "backup create", "documents build", "documents vectors status"} {
		args := strings.Fields(path)
		assertions.True(api.CLIRunCommandAllowed(args), path)
		command, rest, err := rootCmd.Find(args)
		requirements.NoError(err)
		requirements.Empty(rest)
		wire, err := daemonCLIArgsFromCobra(command, nil)
		requirements.NoError(err)
		assertions.Equal(args, wire)
	}
	canonical, _, err := rootCmd.Find([]string{"draft", "reply"})
	requirements.NoError(err)
	callback, found := canonical.GetFlagCompletionFunc("body")
	requirements.True(found)
	values, directive := callback(canonical, nil, "")
	assertions.Equal([]string{"synthetic-completion"}, values)
	assertions.Equal(cobra.ShellCompDirectiveNoFileComp, directive)
	for _, move := range cligrammar.Moves() {
		command, rest, err := rootCmd.Find(cligrammar.Canonical([]string{move.Legacy}))
		requirements.NoError(err)
		assertions.Empty(rest)
		assertions.Equal(move.Canonical, strings.Join(daemonCLICommandPath(command), " "))
	}
}
func TestCLIGrammarPublicDefinitionCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	source := &cobra.Command{}
	fillGrammarDefinition(t, reflect.ValueOf(source).Elem())
	source.Use = "old <id>"
	source.Annotations = map[string]string{"test": "value"}
	source.Flags().String("value", "default", "help")
	require.NoError(source.Flags().SetAnnotation("value", "test", []string{"original"}))
	clone := cloneCommandDefinition(source, "new")
	excluded := map[string]bool{"Use": true, "Aliases": true, "GroupID": true, "Hidden": true, "Deprecated": true}
	before, after := reflect.ValueOf(*source), reflect.ValueOf(*clone)
	for i := range before.NumField() {
		field := before.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		require.False(before.Field(i).IsZero(), "source field %s must exercise copying", field.Name)
		if excluded[field.Name] {
			continue
		}
		a, b := before.Field(i), after.Field(i)
		if a.Kind() == reflect.Func {
			assert.Equal(a.Pointer(), b.Pointer(), field.Name)
		} else {
			assert.Equal(a.Interface(), b.Interface(), field.Name)
		}
	}
	assert.Equal("new <id>", clone.Use)
	clone.Annotations["test"] = "changed"
	clone.Flags().Lookup("value").Annotations["test"][0] = "changed"
	assert.Equal("value", source.Annotations["test"])
	assert.Equal("original", source.Flags().Lookup("value").Annotations["test"][0])
	clone.DisableFlagParsing = false
	require.NoError(clone.ParseFlags([]string{"--value", "updated"}))
	assert.False(source.Flags().Changed("value"))
	assert.True(clone.Flags().Changed("value"))
	value, err := source.Flags().GetString("value")
	require.NoError(err)
	assert.Equal("updated", value)
}

func fillGrammarDefinition(t *testing.T, value reflect.Value) {
	t.Helper()
	switch value.Kind() {
	case reflect.String:
		value.SetString("x")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int:
		value.SetInt(1)
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		fillGrammarDefinition(t, value.Elem())
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		fillGrammarDefinition(t, value.Index(0))
	case reflect.Map:
		key, element := reflect.New(value.Type().Key()).Elem(), reflect.New(value.Type().Elem()).Elem()
		fillGrammarDefinition(t, key)
		fillGrammarDefinition(t, element)
		value.Set(reflect.MakeMap(value.Type()))
		value.SetMapIndex(key, element)
	case reflect.Func:
		value.Set(reflect.MakeFunc(value.Type(), func([]reflect.Value) []reflect.Value {
			results := make([]reflect.Value, value.Type().NumOut())
			for i := range results {
				results[i] = reflect.Zero(value.Type().Out(i))
			}
			return results
		}))
	case reflect.Struct:
		for i := range value.NumField() {
			if value.Type().Field(i).IsExported() {
				fillGrammarDefinition(t, value.Field(i))
			}
		}
	default:
		require.FailNow(t, "new command field needs a nonzero test value", "%s", value.Type())
	}
}

func TestCLIGrammarPrivateRootUnchanged(t *testing.T) {
	root := &cobra.Command{Use: "private"}
	child := &cobra.Command{Use: "draft-reply", RunE: func(*cobra.Command, []string) error { return nil }}
	root.AddCommand(child)
	root.SetArgs([]string{"draft-reply"})
	require.NoError(t, executeRootContext(t.Context(), root))
	assert.Equal(t, []*cobra.Command{child}, slices.DeleteFunc(root.Commands(), func(c *cobra.Command) bool { return c.Name() == "help" || c.Name() == "completion" }))
	assert.False(t, child.Hidden)
}

func TestCLIGrammarPathIdentity(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	requirements := require.New(t)
	assertions := assert.New(t)
	ensureCommandGrammar(rootCmd)
	for _, tc := range []struct {
		path                  string
		configFree, delegated bool
	}{
		{"database version", true, false}, {"version", true, false},
		{"relationship-type update", false, false},
		{"draft get", false, true}, {"draft forward", false, false}, {"draft send-as", false, false},
		{"source calendar add", false, false}, {"source calendar sync", false, false},
		{"calendar", false, true}, {"embeddings __optimize-worker", true, false},
		{"show", false, true}, {"thread show", false, true}, {"account list", false, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			assert := assert.New(t)
			cmd, rest, err := rootCmd.Find(strings.Fields(tc.path))
			require.NoError(t, err)
			require.Empty(t, rest)
			assert.Equal(tc.configFree, skipsConfigLoad(cmd))
			assert.Equal(tc.delegated, agentDelegatedCapable(cmd))
		})
	}
	version, _, err := rootCmd.Find([]string{"database", "version"})
	requirements.NoError(err)
	requirements.NoError(version.Flags().Set("database", "true"))
	assertions.False(skipsConfigLoad(version))
	// Same leaf names on unrelated paths cannot bypass config.
	private := &cobra.Command{Use: "private"}
	group := &cobra.Command{Use: "other"}
	private.AddCommand(group)
	for _, name := range []string{"version", "completion", "openapi", "draft-get", "serve", "muesli-hook"} {
		cmd := &cobra.Command{Use: name}
		group.AddCommand(cmd)
		assertions.False(skipsConfigLoad(cmd), name)
		assertions.False(agentDelegatedCapable(cmd), name)
	}
}

func TestCLIGrammarOldNamesAreNotCLICommands(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	require := require.New(t)
	assert := assert.New(t)
	ensureHelpLayout(rootCmd)
	oldNames := make([]string, 0, len(cligrammar.Moves())+4)
	for _, move := range cligrammar.Moves() {
		oldNames = append(oldNames, move.Legacy)
	}
	oldNames = append(oldNames, "build-embeddings", "build-parquet", "parquet-stats", "sync-calendar-incremental")
	for _, oldName := range oldNames {
		_, _, err := rootCmd.Find([]string{oldName})
		require.Error(err, oldName)
	}
	show, rest, err := rootCmd.Find([]string{"show"})
	require.NoError(err)
	require.Empty(rest)
	assert.Equal("show", show.Name())
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	for _, oldName := range []string{"add-slack", "build-embeddings", "build-parquet"} {
		stdout.Reset()
		stderr.Reset()
		rootCmd.SetArgs([]string{oldName, "--help"})
		require.Error(executeRootContext(t.Context(), rootCmd))
		assert.NotContains(stderr.String(), "deprecated")
	}
}

func TestCLIGrammarImportGroup(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	require := require.New(t)
	assert := assert.New(t)
	ensureHelpLayout(rootCmd)
	badConfig := t.TempDir() + "/config.toml"
	require.NoError(os.WriteFile(badConfig, []byte("[broken"), 0600))
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"--config", badConfig, "import"})
	require.NoError(executeRootContext(t.Context(), rootCmd))
	assert.Contains(output.String(), "whatsapp")
	for _, format := range []string{"whatsapp", "mbox"} {
		cmd, _, err := rootCmd.Find([]string{"import", format})
		require.NoError(err)
		assert.Nil(cmd.InheritedFlags().Lookup("type"))
		err = cmd.ParseFlags([]string{"--type", "whatsapp"})
		require.Error(err)
		assert.Contains(classifyCLIError(cmd, cmd.FlagErrorFunc()(cmd, err)).Hint, "msgvault import "+format+" --help")
	}
}

func TestCLIGrammarChildExecution(t *testing.T) {
	require := require.New(t)
	for _, tc := range []struct{ old, want []string }{
		{[]string{"sync-slack", "example"}, []string{"source", "slack", "sync", "example"}},
		{[]string{"build-cache", "--auto"}, []string{"cache", "build", "--auto"}},
		{[]string{"embeddings", "__optimize-worker"}, []string{"embeddings", "__optimize-worker"}},
	} {
		child, err := newDaemonCLISubprocessCommand(t.Context(), tc.old, nil, "")
		require.NoError(err)
		assert.Equal(t, append([]string{"--no-log-file"}, tc.want...), child.Args[1:])
	}
	cache, err := newBuildCacheSubprocessCommand(t.Context(), buildCacheModeAuto)
	require.NoError(err)
	assert.Equal(t, []string{"--no-log-file", "cache", "build", "--auto"}, cache.Args[1:])
}

func TestCLIGrammarLauncher(t *testing.T) {
	assert := assert.New(t)
	for _, name := range []string{"msgvault-muesli-hook", "msgvault-muesli-hook.exe"} {
		assert.Equal([]string{name, "source", "muesli", "hook"}, LauncherArgs([]string{name}))
		assert.Equal([]string{name, "--help"}, LauncherArgs([]string{name, "--help"}))
	}
	assert.Equal([]string{"msgvault"}, LauncherArgs([]string{"msgvault"}))
	assert.Empty(LauncherArgs(nil))
}

func TestCLIGrammarCompletion(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	require := require.New(t)
	assert := assert.New(t)
	original := directCommand(rootCmd, "draft-reply")
	require.NoError(original.RegisterFlagCompletionFunc("body", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"synthetic-completion"}, cobra.ShellCompDirectiveNoFileComp
	}))
	ensureHelpLayout(rootCmd)
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"__complete", "draft", "reply", "--body", ""})
	require.NoError(executeRootContext(t.Context(), rootCmd))
	assert.Contains(stdout.String(), "synthetic-completion")
	assert.Contains(stdout.String(), ":4")
	assert.NotContains(stderr.String(), "deprecated")
}

func TestCLIGrammarCanonicalFlags(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	require := require.New(t)
	assert := assert.New(t)
	ensureHelpLayout(rootCmd)
	gmail, _, err := rootCmd.Find([]string{"source", "gmail", "add"})
	require.NoError(err)
	require.NoError(gmail.ParseFlags([]string{"--grant-decided", "--readonly=false"}))
	wire, err := daemonCLIArgsFromCobra(gmail, []string{"user@example.com"})
	require.NoError(err)
	assert.Equal([]string{"add-account", "--readonly=false", "user@example.com"}, wire)
	slack, _, err := rootCmd.Find([]string{"source", "slack", "sync"})
	require.NoError(err)
	require.NoError(slack.ParseFlags([]string{"--build-cache", "--no-build-cache"}))
	require.Error(slack.ValidateFlagGroups())
	whatsapp, _, err := rootCmd.Find([]string{"import", "whatsapp"})
	require.NoError(err)
	require.ErrorContains(whatsapp.ValidateRequiredFlags(), "phone")
	compose, _, err := rootCmd.Find([]string{"draft", "compose"})
	require.NoError(err)
	require.NoError(compose.ParseFlags([]string{"--to", "first@example.com", "--to", "second@example.com"}))
	wire, err = daemonCLIArgsFromCobra(compose, nil)
	require.NoError(err)
	assert.Equal([]string{"draft-compose", "--to=first@example.com", "--to=second@example.com"}, wire)
	selected, _, err := rootCmd.Find([]string{"sync", "--", "full"})
	require.NoError(err)
	assert.Equal("sync", selected.Name())
	selected, rest, err := rootCmd.Find([]string{"import", "./whatsapp", "--type", "whatsapp"})
	require.NoError(err)
	assert.Same(importCmd, selected)
	assert.Contains(rest, "./whatsapp")
}

func TestCLIGrammarCanonicalHelpDoesNotRepeatParentPath(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"account", "export-token", "--help"})
	require.NoError(t, executeRootContext(t.Context(), rootCmd))
	assert.Contains(t, output.String(), "msgvault account export-token you@example.com")
	assert.NotContains(t, output.String(), "msgvault account account export-token")
}

func TestCLIGrammarDatabaseVersionLoadsConfig(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	require := require.New(t)
	ensureHelpLayout(rootCmd)
	bad := t.TempDir() + "/config.toml"
	require.NoError(os.WriteFile(bad, []byte("[broken"), 0600))
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(&output)
	rootCmd.SetArgs([]string{"--config", bad, "database", "version", "--database"})
	require.Error(executeRootContext(t.Context(), rootCmd))
	assert.NotEqual(t, strconv.Itoa(store.SchemaVersion)+"\n", output.String())
}

func TestCLIGrammarLauncherTree(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs(append(LauncherArgs([]string{"msgvault-muesli-hook"})[1:], "--help"))
	require.NoError(t, executeRootContext(t.Context(), rootCmd))
	assert.Contains(t, stdout.String(), "msgvault source muesli hook")
	assert.Empty(t, stderr.String())
}

func TestCLIGrammarServeOverridesUseFullPath(t *testing.T) {
	for _, nested := range []bool{false, true} {
		require := require.New(t)
		assert := assert.New(t)
		root := &cobra.Command{Use: "msgvault"}
		parent := root
		if nested {
			parent = &cobra.Command{Use: "other"}
			root.AddCommand(parent)
		}
		command := &cobra.Command{Use: "serve"}
		parent.AddCommand(command)
		addServeConfigFlags(command)
		require.NoError(command.ParseFlags([]string{"--bind", "127.0.0.1", "--port", "8181"}))
		override := serveRuntimeOverrides(command)
		if nested {
			assert.Nil(override.BindAddr)
			assert.Nil(override.APIPort)
		} else {
			require.NotNil(override.BindAddr)
			require.NotNil(override.APIPort)
			assert.Equal("127.0.0.1", *override.BindAddr)
			assert.Equal(8181, *override.APIPort)
		}
	}
}

func TestCLIGrammarFlagOwnership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := &cobra.Command{Use: "example"}
	root.PersistentFlags().Bool("global", false, "global flag")
	source := &cobra.Command{Use: "old"}
	root.AddCommand(source)
	source.Flags().String("local", "local default", "local flag")
	source.PersistentFlags().String("scoped", "scoped default", "command's own persistent flag")
	require.NoError(source.RegisterFlagCompletionFunc("scoped", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"scoped-completion"}, cobra.ShellCompDirectiveNoFileComp
	}))
	// Cobra's normal metadata inspection merges inherited and persistent flags
	// into Flags. They must not become cloned local flags or duplicate callbacks.
	source.LocalFlags()
	clone := cloneCommandDefinition(source, "new")
	assert.Nil(clone.Flags().Lookup("global"))
	assert.Nil(clone.LocalNonPersistentFlags().Lookup("scoped"))
	require.NotNil(clone.PersistentFlags().Lookup("scoped"))
	assert.Same(source.PersistentFlags().Lookup("scoped").Value, clone.PersistentFlags().Lookup("scoped").Value)
	callback, ok := clone.GetFlagCompletionFunc("scoped")
	require.True(ok)
	values, directive := callback(clone, nil, "")
	assert.Equal([]string{"scoped-completion"}, values)
	assert.Equal(cobra.ShellCompDirectiveNoFileComp, directive)
}
