//go:build darwin

// codexseatbeltprobe stands in for the Codex app server under the macOS
// launcher. It reads one JSON operation per stdin line, attempts it, and
// writes one JSON result per stdout line, so a test can observe exactly what
// any code in the contained process can and cannot reach.
package main

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <unistd.h>
#include <CoreFoundation/CoreFoundation.h>

// shm_open is variadic, which cgo cannot call directly.
static int probe_shm_open(const char *name, int create) {
	int fd = create ? shm_open(name, O_RDWR | O_CREAT, 0600) : shm_open(name, O_RDONLY);
	if (fd < 0) {
		return -errno;
	}
	if (create && ftruncate(fd, 64) != 0) {
		int saved = errno;
		close(fd);
		return -saved;
	}
	close(fd);
	return 0;
}

static int probe_preference_sync(const char *domain) {
	CFStringRef d = CFStringCreateWithCString(NULL, domain, kCFStringEncodingUTF8);
	Boolean ok = CFPreferencesAppSynchronize(d);
	CFRelease(d);
	return ok ? 1 : 0;
}

static int probe_preference_present(const char *key, const char *domain) {
	CFStringRef k = CFStringCreateWithCString(NULL, key, kCFStringEncodingUTF8);
	CFStringRef d = CFStringCreateWithCString(NULL, domain, kCFStringEncodingUTF8);
	CFPropertyListRef value = CFPreferencesCopyAppValue(k, d);
	CFRelease(k);
	CFRelease(d);
	if (value == NULL) {
		return 0;
	}
	CFRelease(value);
	return 1;
}
*/
import "C"

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const probeVersion = "codex-cli 0.0.0-seatbelt-probe"

type operation struct {
	Op        string `json:"op"`
	Path      string `json:"path"`
	Network   string `json:"network"`
	Address   string `json:"address"`
	Authority string `json:"authority"`
	Domain    string `json:"domain"`
	Key       string `json:"key"`
}

type result struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Value string `json:"value,omitempty"`
}

func main() {
	// The launch gate reads the version before it admits the snapshot.
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(probeVersion)
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var op operation
		if err := json.Unmarshal(scanner.Bytes(), &op); err != nil {
			os.Exit(2)
		}
		value, err := run(op)
		reply := result{OK: err == nil, Value: value}
		if err != nil {
			reply.Error = err.Error()
		}
		if err := encoder.Encode(reply); err != nil {
			os.Exit(2)
		}
	}
}

func run(op operation) (string, error) {
	switch op.Op {
	case "read":
		contents, err := os.ReadFile(op.Path)
		return string(contents), err
	case "list":
		entries, err := os.ReadDir(op.Path)
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return strings.Join(names, "\n"), err
	case "write":
		return "", os.WriteFile(op.Path, []byte("SYNTHETIC_PROBE_WRITE"), 0o600)
	case "exec":
		return "", exec.Command(op.Path).Run() //nolint:gosec,noctx // The test names a fixed system binary.
	case "env":
		names := []string{}
		for _, item := range os.Environ() {
			name, _, _ := strings.Cut(item, "=")
			names = append(names, name)
		}
		slices.Sort(names)
		return strings.Join(names, "\n"), nil
	case "dial":
		conn, err := net.DialTimeout(op.Network, op.Address, 2*time.Second)
		if err != nil {
			return "", err
		}
		if op.Network == "udp" {
			_, err = conn.Write([]byte("SYNTHETIC_PROBE_DATAGRAM"))
		}
		return "", errors.Join(err, conn.Close())
	case "connect":
		return connect(op.Authority)
	case "shm", "shm-create":
		name := C.CString(op.Path)
		defer C.free(unsafe.Pointer(name))
		create := C.int(0)
		if op.Op == "shm-create" {
			create = 1
		}
		if code := C.probe_shm_open(name, create); code != 0 {
			return "", syscall.Errno(-code)
		}
		return "", nil
	case "shm-unlink":
		name := C.CString(op.Path)
		defer C.free(unsafe.Pointer(name))
		if C.shm_unlink(name) != 0 {
			return "", errors.New("shm_unlink failed")
		}
		return "", nil
	case "preference-sync":
		domain := C.CString(op.Domain)
		defer C.free(unsafe.Pointer(domain))
		if C.probe_preference_sync(domain) == 0 {
			return "", errors.New("preference synchronize failed")
		}
		return "", nil
	case "preference":
		key := C.CString(op.Key)
		domain := C.CString(op.Domain)
		defer C.free(unsafe.Pointer(key))
		defer C.free(unsafe.Pointer(domain))
		if C.probe_preference_present(key, domain) == 0 {
			return "", errors.New("preference unavailable")
		}
		return "present", nil
	}
	return "", fmt.Errorf("unknown operation %q", op.Op)
}

// connect opens a tunnel through HTTPS_PROXY as the Codex HTTP client does,
// sending the proxy credential from the URL, and returns the status line.
func connect(authority string) (string, error) {
	proxy, err := url.Parse(os.Getenv("HTTPS_PROXY"))
	if err != nil || proxy.Host == "" {
		return "", errors.New("HTTPS_PROXY is unset")
	}
	conn, err := net.DialTimeout("tcp", proxy.Host, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	request := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n"
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		request += "Proxy-Authorization: Basic " + basicCredential(proxy.User.Username(), password) + "\r\n"
	}
	if _, err := conn.Write([]byte(request + "\r\n")); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line), err
}

func basicCredential(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}
