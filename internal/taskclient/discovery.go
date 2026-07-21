package taskclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxDescriptorBytes = int64(64 << 10)
	maxTokenBytes      = int64(16 << 10)
)

type descriptor struct {
	ProtocolVersion string `json:"protocol_version"`
	InstanceID      string `json:"instance_id"`
	Endpoint        string `json:"endpoint"`
	TokenFile       string `json:"token_file,omitempty"`
}

type DiscoveryOptions struct {
	DescriptorPath   string
	APIKey           string
	Timeout          time.Duration
	MaxResponseBytes int64
	HTTPClient       *http.Client
	// platformSecurityCheck is a package-local test seam. Production callers
	// always use descriptorFileSecurityCheck.
	platformSecurityCheck func() error
}

func Discover(_ context.Context, options DiscoveryOptions) (*Client, error) {
	path := strings.TrimSpace(options.DescriptorPath)
	if path == "" {
		path = DefaultDescriptorPath()
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	securityCheck := options.platformSecurityCheck
	if securityCheck == nil {
		securityCheck = descriptorFileSecurityCheck
	}
	if err := securityCheck(); err != nil {
		return nil, err
	}
	expectedOwner := currentUserID()
	data, err := readSecureRegularFile(path, expectedOwner, maxDescriptorBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var value descriptor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: malformed descriptor", ErrIncompatible)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing descriptor data", ErrIncompatible)
	}
	if value.ProtocolVersion != ProtocolVersion || strings.TrimSpace(value.InstanceID) == "" {
		return nil, fmt.Errorf("%w: unsupported descriptor protocol", ErrIncompatible)
	}

	apiKey := strings.TrimSpace(options.APIKey)
	_, kind, _, err := validateEndpoint(strings.TrimSpace(value.Endpoint))
	if err != nil {
		return nil, err
	}
	if kind == EndpointLoopbackHTTP && apiKey == "" && value.TokenFile != "" {
		if !filepath.IsAbs(value.TokenFile) {
			return nil, fmt.Errorf("%w: token file must be absolute", ErrInsecureDescriptor)
		}
		token, tokenErr := readSecureRegularFile(value.TokenFile, expectedOwner, maxTokenBytes)
		if tokenErr != nil {
			return nil, tokenErr
		}
		apiKey = strings.TrimSpace(string(token))
	}
	if kind == EndpointLoopbackHTTP && apiKey == "" {
		return nil, ErrAuthenticationRequired
	}
	client, err := New(ClientOptions{
		Endpoint:         value.Endpoint,
		APIKey:           apiKey,
		Timeout:          options.Timeout,
		MaxResponseBytes: options.MaxResponseBytes,
		HTTPClient:       options.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	client.instanceID = value.InstanceID
	return client, nil
}

func DefaultDescriptorPath() string {
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" && filepath.IsAbs(runtimeDir) {
		return filepath.Join(runtimeDir, "msgvault", "task-integration.json")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("msgvault-%d", currentUserID()), "task-integration.json")
}

func validateSecureRegularFile(path string, expectedOwner uint32) error {
	file, err := openSecureRegularFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("%w: open secure file", ErrInsecureDescriptor)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: inspect secure file", ErrInsecureDescriptor)
	}
	return validateSecureFileInfo(info, expectedOwner)
}

func readSecureRegularFile(path string, expectedOwner uint32, maximum int64) ([]byte, error) {
	file, err := openSecureRegularFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("%w: open secure file", ErrInsecureDescriptor)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: inspect secure file", ErrInsecureDescriptor)
	}
	if err := validateSecureFileInfo(info, expectedOwner); err != nil {
		return nil, err
	}
	return readBounded(file, maximum)
}

func validateSecureFileInfo(info os.FileInfo, expectedOwner uint32) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: file must be regular and non-symlinked", ErrInsecureDescriptor)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: file permissions must deny group and other access", ErrInsecureDescriptor)
	}
	owner, err := fileInfoOwnerID(info)
	if err != nil || owner != expectedOwner {
		return fmt.Errorf("%w: file owner does not match daemon user", ErrInsecureDescriptor)
	}
	return nil
}
