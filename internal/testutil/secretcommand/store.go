package secretcommand

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Run implements the test-only external store protocol without private store
// dependencies. Tests assert stored bytes and production results, not fake calls.
func Run() {
	args := os.Args
	if len(args) < 2 {
		os.Exit(2)
	}
	mode := args[1]
	root := os.Getenv("MSGVAULT_TEST_SECRET_ROOT")
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(os.Getenv("MSGVAULT_TOKEN_PATH"))))
	path := filepath.Join(root, key)
	switch mode {
	case "read", "read-once":
		if mode == "read-once" {
			if _, err := os.Stat(path + ".read"); err == nil { //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
				os.Exit(7)
			}
			_ = os.WriteFile(path+".read", nil, 0600) //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
		}
		data, err := os.ReadFile(path) //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
		if os.IsNotExist(err) {
			os.Exit(3)
		}
		if err != nil {
			os.Exit(2)
		}
		_, _ = os.Stdout.Write(data)
	case "write":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		if os.WriteFile(path, data, 0600) != nil { //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
			os.Exit(2)
		}
		_ = os.WriteFile(path+".account", []byte(os.Getenv("MSGVAULT_ACCOUNT")), 0600) //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
		_ = os.WriteFile(path+".dir", []byte(os.Getenv("MSGVAULT_TOKEN_DIR")), 0600)   //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
	case "delete":
		_ = os.Remove(path)              //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
		_ = os.Remove(path + ".account") //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
		_ = os.Remove(path + ".dir")     //nolint:gosec // path uses a SHA-256 key beneath the fixture's private temp directory.
	case "list":
		entries, _ := os.ReadDir(root)
		var values []string
		for _, entry := range entries {
			if name, ok := strings.CutSuffix(entry.Name(), ".account"); ok {
				dir, _ := os.ReadFile(filepath.Join(root, name+".dir")) //nolint:gosec // name came from ReadDir and is a single path component under the fixture's private temp directory.
				if string(dir) == os.Getenv("MSGVAULT_TOKEN_DIR") {
					data, _ := os.ReadFile(filepath.Join(root, entry.Name())) //nolint:gosec // entry.Name came from ReadDir and is a single path component under the fixture's private temp directory.
					values = append(values, string(data))
				}
			}
		}
		if values == nil {
			values = []string{}
		}
		data, _ := json.Marshal(values)
		_, _ = os.Stdout.Write(data)
	case "echo":
		_, _ = fmt.Fprint(os.Stdout, args[2])
	case "stdin":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "client":
		_, _ = fmt.Fprint(os.Stdout, `{"installed":{"client_id":"example-client","client_secret":"example-secret","redirect_uris":["http://localhost"]}}`)
	case "fail":
		_, _ = fmt.Fprint(os.Stdout, "example-private-output")
		_, _ = fmt.Fprint(os.Stderr, "example-private-error")
		os.Exit(7)
	case "empty":
	case "empty-object":
		_, _ = fmt.Fprint(os.Stdout, "{}")
	case "overflow":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 2<<20))
	case "wait":
		_ = os.WriteFile(filepath.Join(root, "started"), nil, 0600) //nolint:gosec // root is the fixture's private temp directory.
		time.Sleep(2 * time.Minute)
	case "hold-pipe":
		child := exec.Command(os.Args[0], "wait") //nolint:gosec // Launch only this fixture executable.
		child.Stdout = os.Stdout
		if child.Start() != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(filepath.Join(root, "child-pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600) //nolint:gosec // root is the fixture's private temp directory.
		_ = child.Process.Release()
	default:
		os.Exit(2)
	}
	os.Exit(0)
}
