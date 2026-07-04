package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/update"
	"golang.org/x/crypto/argon2"
)

const (
	daemonService           = "msgvault"
	daemonAPIVersion        = 1
	defaultDaemonBindAddr   = "127.0.0.1"
	runtimeHost             = "host"
	runtimePort             = "port"
	runtimeAPIVersion       = "api_version"
	runtimeAPISchemaVersion = "api_schema_version"
	runtimeAuthFingerprint  = "auth_fingerprint"
	runtimeCreateTime       = "create_time"
	runtimeShutdownToken    = "shutdown_token"
	daemonProbeTick         = 250 * time.Millisecond
)

var daemonAuthFingerprintSalt = []byte("msgvault-daemon-auth-fingerprint-v1")

type DaemonRuntime struct {
	Record           daemon.RuntimeRecord
	Host             string
	Port             int
	API              int
	APISchemaVersion string
}

func daemonRuntimeStore(dataDir string) daemon.RuntimeStore {
	return daemon.RuntimeStore{Dir: dataDir}
}

func writeDaemonRuntime(dataDir string, host string, port int, version string, apiKey string) (string, string, error) {
	shutdownToken, err := newDaemonShutdownToken()
	if err != nil {
		return "", "", fmt.Errorf("create shutdown token: %w", err)
	}
	ep := daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(probeHostForDial(host), strconv.Itoa(port)),
	}
	rec := daemon.NewRuntimeRecord(daemonService, version, ep)
	rec.Metadata = map[string]string{
		runtimeHost:             host,
		runtimePort:             strconv.Itoa(port),
		runtimeAPIVersion:       strconv.Itoa(daemonAPIVersion),
		runtimeAPISchemaVersion: api.APISchemaVersion,
		runtimeAuthFingerprint:  daemonAPIKeyFingerprint(apiKey),
		runtimeShutdownToken:    shutdownToken,
	}
	if createTime, ok := processCreateTimeMillis(os.Getpid()); ok {
		rec.Metadata[runtimeCreateTime] = strconv.FormatInt(createTime, 10)
	}
	path, err := daemonRuntimeStore(dataDir).Write(rec)
	if err != nil {
		return "", "", fmt.Errorf("write daemon runtime record: %w", err)
	}
	return path, shutdownToken, nil
}

func daemonAPIKeyFingerprint(apiKey string) string {
	if apiKey == "" {
		return "none"
	}
	key := argon2.IDKey([]byte(apiKey), daemonAuthFingerprintSalt, 1, 8*1024, 1, 32)
	return "argon2id:v1:" + hex.EncodeToString(key)
}

func newDaemonShutdownToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random shutdown token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func removeDaemonRuntime(dataDir string) {
	path, err := daemonRuntimeStore(dataDir).Path(os.Getpid())
	if err == nil {
		_ = os.Remove(path)
	}
}

func findDaemonRuntime(dataDir string) *DaemonRuntime {
	rt, err := findCompatibleDaemonRuntime(dataDir)
	if err != nil {
		return nil
	}
	return rt
}

func findCompatibleDaemonRuntime(dataDir string) (*DaemonRuntime, error) {
	return findCompatibleDaemonRuntimeContext(context.Background(), dataDir)
}

func findCompatibleDaemonRuntimeContext(ctx context.Context, dataDir string) (*DaemonRuntime, error) {
	rt, _, err := findRespondingDaemonRuntime(ctx, dataDir, func(_ *DaemonRuntime, compatErr error) bool {
		return compatErr == nil
	})
	return rt, err
}

// findAnyDaemonRuntime returns a responding daemon runtime for dataDir,
// compatible with this client or not. Guards that only need to know whether
// a live process owns the archive (like the restore-into-home refusal) must
// use this rather than findDaemonRuntime: a daemon left running across a
// CLI upgrade or downgrade fails the compatibility check, yet it still holds
// the database open.
func findAnyDaemonRuntime(dataDir string) *DaemonRuntime {
	// findRespondingDaemonRuntime returns the accepted runtime's
	// compatibility error alongside it; an incompatible daemon is exactly
	// what this lookup must still surface, so only found matters here.
	rt, found, _ := findRespondingDaemonRuntime(context.Background(), dataDir,
		func(*DaemonRuntime, error) bool { return true })
	if !found {
		return nil
	}
	return rt
}

func findIncompatibleDaemonRuntime(dataDir string) (*DaemonRuntime, bool, error) {
	rt, found, err := findRespondingDaemonRuntime(context.Background(), dataDir, func(_ *DaemonRuntime, compatErr error) bool {
		return compatErr != nil
	})
	if err != nil {
		return rt, found, err
	}
	return nil, false, nil
}

func findRespondingDaemonRuntime(
	ctx context.Context,
	dataDir string,
	accept func(*DaemonRuntime, error) bool,
) (*DaemonRuntime, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	records, err := listLiveDaemonRuntimeRecords(dataDir)
	if err != nil {
		return nil, false, err
	}
	for _, rec := range records {
		info, err := probeDaemonRuntimeRecord(ctx, rec)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, false, ctxErr
			}
			continue
		}
		if info.PID != rec.PID {
			continue
		}
		rt := daemonRuntimeFromRecord(rec)
		compatErr := daemonRuntimeCompatibilityError(rt)
		if accept(rt, compatErr) {
			return rt, true, compatErr
		}
	}
	return nil, false, nil
}

func listLiveDaemonRuntimeRecords(dataDir string) ([]daemon.RuntimeRecord, error) {
	store := daemonRuntimeStore(dataDir)
	_, _ = store.CleanupDead()
	records, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("list daemon runtimes: %w", err)
	}
	alive := make([]daemon.RuntimeRecord, 0, len(records))
	for _, rec := range records {
		if rec.Service != "" && rec.Service != daemonService {
			continue
		}
		if !daemon.ProcessAlive(rec.PID) {
			continue
		}
		if runtimeRecordHasMismatchedCreateTime(store, rec) {
			continue
		}
		alive = append(alive, rec)
	}
	return alive, nil
}

func daemonRuntimeFromRecord(rec daemon.RuntimeRecord) *DaemonRuntime {
	ep := rec.Endpoint()
	host, portText, _ := net.SplitHostPort(ep.Address)
	port, _ := strconv.Atoi(portText)
	apiVersion := 0
	apiSchemaVersion := ""
	if rec.Metadata != nil {
		if h := rec.Metadata[runtimeHost]; h != "" {
			host = h
		}
		if p := rec.Metadata[runtimePort]; p != "" {
			if parsed, err := strconv.Atoi(p); err == nil {
				port = parsed
			}
		}
		apiVersion, _ = strconv.Atoi(rec.Metadata[runtimeAPIVersion])
		apiSchemaVersion = rec.Metadata[runtimeAPISchemaVersion]
	}
	return &DaemonRuntime{
		Record:           rec,
		Host:             host,
		Port:             port,
		API:              apiVersion,
		APISchemaVersion: apiSchemaVersion,
	}
}

func daemonRuntimeCompatibilityError(rt *DaemonRuntime) error {
	if rt == nil {
		return nil
	}
	if rt.API != daemonAPIVersion {
		return fmt.Errorf(
			"daemon API version %d is incompatible with client API version %d",
			rt.API, daemonAPIVersion,
		)
	}
	if err := apiSchemaCompatibilityError(rt.APISchemaVersion); err != nil {
		return err
	}
	return nil
}

func apiSchemaCompatibilityError(peerVersion string) error {
	if peerVersion == "" {
		return nil
	}
	peerMajor, ok := apiSchemaMajor(peerVersion)
	if !ok {
		return fmt.Errorf("daemon API schema version %q is invalid", peerVersion)
	}
	currentMajor, ok := apiSchemaMajor(api.APISchemaVersion)
	if !ok {
		return fmt.Errorf("client API schema version %q is invalid", api.APISchemaVersion)
	}
	if peerMajor != currentMajor {
		return fmt.Errorf(
			"daemon API schema version %q is incompatible with client API schema version %q",
			peerVersion, api.APISchemaVersion,
		)
	}
	return nil
}

func apiSchemaMajor(version string) (int, bool) {
	for i, r := range version {
		if r == '.' {
			version = version[:i]
			break
		}
	}
	major, err := strconv.Atoi(version)
	return major, err == nil
}

func probeRuntime(
	ctx context.Context,
	rec daemon.RuntimeRecord,
	opts daemon.ProbeOptions,
) (daemon.PingInfo, error) {
	info, err := daemon.Probe(ctx, rec.Endpoint(), opts)
	if err != nil {
		return daemon.PingInfo{}, fmt.Errorf("probe daemon runtime: %w", err)
	}
	return info, nil
}

func probeDaemonRuntimeRecord(ctx context.Context, rec daemon.RuntimeRecord) (daemon.PingInfo, error) {
	return probeRuntime(ctx, rec, daemon.ProbeOptions{
		ExpectedService: daemonService,
		Timeout:         500 * time.Millisecond,
	})
}

func runtimeRecordHasMismatchedCreateTime(
	store daemon.RuntimeStore,
	rec daemon.RuntimeRecord,
) bool {
	if rec.Metadata == nil {
		return false
	}
	recorded := rec.Metadata[runtimeCreateTime]
	if recorded == "" || processCreateTimeMatches(rec.PID, recorded) {
		return false
	}
	if path, err := store.Path(rec.PID); err == nil {
		_ = os.Remove(path)
	}
	return true
}

func processCreateTimeMillis(pid int) (int64, bool) {
	const maxInt32 = 1<<31 - 1
	if pid <= 0 || pid > maxInt32 {
		return 0, false
	}
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, false
	}
	created, err := proc.CreateTime()
	if err != nil {
		return 0, false
	}
	return created, true
}

func processCreateTimeMatches(pid int, recordedMillis string) bool {
	recorded, err := strconv.ParseInt(recordedMillis, 10, 64)
	if err != nil {
		return false
	}
	live, ok := processCreateTimeMillis(pid)
	return ok && live == recorded
}

func probeHostForDial(host string) string {
	switch host {
	case "", "0.0.0.0":
		return defaultDaemonBindAddr
	case "::":
		return "::1"
	default:
		return host
	}
}

func urlFromDaemonRuntime(rt *DaemonRuntime) string {
	if rt == nil {
		return ""
	}
	return "http://" + net.JoinHostPort(probeHostForDial(rt.Host), strconv.Itoa(rt.Port))
}

func shouldUpgradeDaemonRuntimeWithPolicy(rt *DaemonRuntime, currentVersion string, policy string) bool {
	if rt == nil {
		return false
	}
	switch policy {
	case "", config.DaemonAutoRestartNewer:
		return shouldUpgradeDaemonRuntimeToNewer(rt, currentVersion)
	case config.DaemonAutoRestartNever:
		return false
	case config.DaemonAutoRestartAlways:
		return rt.Record.Version != currentVersion
	default:
		return shouldUpgradeDaemonRuntimeToNewer(rt, currentVersion)
	}
}

func shouldUpgradeDaemonRuntimeToNewer(rt *DaemonRuntime, currentVersion string) bool {
	if rt.Record.Version == "" {
		return !update.IsDevBuildVersion(currentVersion)
	}
	return update.IsNewer(currentVersion, rt.Record.Version)
}

func shouldUpgradeIncompatibleDaemonRuntimeWithPolicy(rt *DaemonRuntime, currentVersion string, policy string) bool {
	if !shouldUpgradeDaemonRuntimeWithPolicy(rt, currentVersion, policy) {
		return false
	}
	if rt.APISchemaVersion != "" {
		peerMajor, peerOK := apiSchemaMajor(rt.APISchemaVersion)
		currentMajor, currentOK := apiSchemaMajor(api.APISchemaVersion)
		if peerOK && currentOK && peerMajor > currentMajor {
			return false
		}
	}
	return rt.API <= daemonAPIVersion
}
