package scripts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPackageShardsWindows(t *testing.T) {
	r := require.New(t)
	script, err := filepath.Abs("test-package-shards.ps1")
	r.NoError(err)
	pwsh, err := exec.LookPath("pwsh")
	r.NoError(err)
	dir := filepath.Join(t.TempDir(), "package with spaces")
	r.NoError(os.Mkdir(dir, 0o700))
	r.NoError(os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module shardfixture\n\ngo 1.25\n"), 0o600))

	var source strings.Builder
	source.WriteString(`package shardfixture
import ("flag"; "fmt"; "os"; "strings"; "testing"; "time")
var output *os.File
func TestMain(m *testing.M) {
    flag.Parse()
    code := m.Run()
    if output != nil { if err := output.Close(); err != nil { panic(err) } }
    os.Exit(code)
}
func record(t *testing.T) {
    if os.Getenv("SHARD_COORDINATE") != "" {
        signal := os.Getenv("SHARD_OUTPUT") + "/released"
        if strings.HasPrefix(t.Name(), "Test1199_") {
            if err := os.WriteFile(signal, nil, 0600); err != nil { panic(err) }
        }
        if strings.HasPrefix(t.Name(), "Test0000_") {
            for {
                if _, err := os.Stat(signal); err == nil { break } else if !os.IsNotExist(err) { panic(err) }
                time.Sleep(10 * time.Millisecond)
            }
        }
    }
    if output == nil {
        var err error
        output, err = os.Create(fmt.Sprintf("%s/%d", os.Getenv("SHARD_OUTPUT"), os.Getpid()))
        if err != nil { panic(err) }
        if _, err = fmt.Fprintln(output, flag.Lookup("test.timeout").Value); err != nil { panic(err) }
    }
    if _, err := fmt.Fprintln(output, t.Name()); err != nil { panic(err) }
    if t.Name() == os.Getenv("SHARD_FAIL") { panic("shard failure sentinel") }
}
`)
	const count = 1200
	want := make([]string, count)
	for i := range want {
		want[i] = fmt.Sprintf("Test%04d_%s", i, strings.Repeat("long_name_", 13))
		fmt.Fprintf(&source, "func %s(t *testing.T) { record(t) }\n", want[i])
	}
	r.NoError(os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte(source.String()), 0o600))

	run := func(t *testing.T, timeout, fail string) (string, string, error) {
		t.Helper()
		output := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, pwsh, "-NoProfile", "-File", script, "-Package", ".", "-ShardCount", "4", "-Tags", "fts5 sqlite_vec", "-Timeout", timeout)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "SHARD_OUTPUT="+output, "SHARD_FAIL="+fail)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		result, err := cmd.CombinedOutput()
		return output, string(result), err
	}

	for _, timeout := range []string{"1m30s", "0"} {
		t.Run(timeout, func(t *testing.T) {
			require := require.New(t)
			output, result, err := run(t, timeout, "")
			require.NoError(err, result)
			files, err := os.ReadDir(output)
			require.NoError(err)
			require.Greater(len(files), 4, "fixture must exceed one command line per shard")
			var got []string
			budgets := make([]map[int]time.Duration, 4)
			for i := range budgets {
				budgets[i] = make(map[int]time.Duration)
			}
			for _, file := range files {
				data, err := os.ReadFile(filepath.Join(output, file.Name()))
				require.NoError(err)
				lines := strings.Fields(string(data))
				require.Greater(len(lines), 1)
				budget, err := time.ParseDuration(lines[0])
				require.NoError(err)
				first, err := strconv.Atoi(lines[1][4:8])
				require.NoError(err)
				budgets[first%4][first] = budget
				for _, name := range lines[1:] {
					index, err := strconv.Atoi(name[4:8])
					require.NoError(err)
					require.Equal(first%4, index%4, "batches must retain shard assignments")
				}
				got = append(got, lines[1:]...)
			}
			sort.Strings(got)
			require.Equal(want, got, "every test must run exactly once")
			for _, batches := range budgets {
				var starts []int
				for start := range batches {
					starts = append(starts, start)
				}
				sort.Ints(starts)
				previous := 90 * time.Second
				for i, start := range starts {
					if timeout == "0" {
						require.Zero(batches[start])
					} else if i == 0 {
						require.Equal(previous, batches[start])
					} else {
						require.Positive(batches[start])
						require.Less(batches[start], previous)
					}
					previous = batches[start]
				}
			}
		})
	}
	t.Run("independent shards", func(t *testing.T) {
		require := require.New(t)
		t.Setenv("SHARD_COORDINATE", "1")
		output, result, err := run(t, "30s", "")
		require.NoError(err, result)
		require.FileExists(filepath.Join(output, "released"))
	})
	t.Run("later batch failure", func(t *testing.T) {
		require := require.New(t)
		_, result, err := run(t, "1m30s", want[count-1])
		require.Error(err)
		require.Contains(result, "shard failure sentinel")
	})
}
