package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"go.kenn.io/kit/pack"
)

// backupFreezeWatchdogTimeout bounds how long a backup freeze may hold the
// operation gate without a matching End call. A crashed or hung backup
// subprocess would otherwise wedge the daemon's gate forever; the watchdog
// auto-releases the gate and logs at error level so the daemon self-heals.
// Package var so tests can shorten it.
var backupFreezeWatchdogTimeout = 60 * time.Second

const (
	backupFreezeBeginPath = "/api/v1/backup/freeze/begin"
	backupFreezeEndPath   = "/api/v1/backup/freeze/end"
)

// backupFreezeState tracks the single active backup freeze window, if any.
// The backup subprocess calls Begin, checkpoints and pins its own SQLite
// session, then calls End; the daemon's operation gate stays held across that
// window. Attachment capture runs after the gate is released and may overlap
// daemon-owned attachment maintenance.
type backupFreezeState struct {
	mu sync.Mutex
	// active is set as soon as Begin reserves the window, before the
	// (possibly blocking) gate acquisition completes, so a concurrent second
	// Begin is rejected immediately instead of racing to acquire the gate.
	active   bool
	token    string
	release  func()
	watchdog *time.Timer
}

// backupFreezeLocalAddrs enumerates this machine's interface addresses.
// Package var so tests can stub the interface list.
var backupFreezeLocalAddrs = net.InterfaceAddrs

// isSameHostRequest reports whether r originated on this machine: from a
// loopback address, or from an address assigned to one of this host's own
// interfaces. The second case matters because backup create's freeze
// subprocess dials the daemon's configured bind address — a daemon bound to
// a specific non-loopback address (a NAS bound to its LAN IP) sees
// same-host connections arrive with that address as the source, not
// loopback. Uses r.RemoteAddr, never forwarded-for headers, so a remote
// client cannot spoof it; freeze handlers additionally require API auth.
func isSameHostRequest(r *http.Request) bool {
	if isLoopbackRequest(r) {
		return true
	}
	ip := net.ParseIP(clientIP(r))
	if ip == nil {
		return false
	}
	addrs, err := backupFreezeLocalAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		var ifIP net.IP
		switch a := addr.(type) {
		case *net.IPNet:
			ifIP = a.IP
		case *net.IPAddr:
			ifIP = a.IP
		}
		if ifIP != nil && ifIP.Equal(ip) {
			return true
		}
	}
	return false
}

type backupFreezeBeginResponse struct {
	Token string `json:"token"`
}

type backupFreezeEndRequest struct {
	Token string `json:"token"`
}

type backupFreezeEndResponse struct{}

// handleBackupFreezeBegin opens a backup freeze window: it acquires the
// daemon's operation gate itself (the gate middleware exempts this path; see
// operationGateExemptPaths) and returns a token identifying the window. The
// caller must present that token to End. A second Begin while a freeze is
// already active is rejected without touching the gate.
func (s *Server) handleBackupFreezeBegin(w http.ResponseWriter, r *http.Request) {
	if !isSameHostRequest(r) || !s.apiRequestAuthorized(r) {
		writeError(w, http.StatusNotFound, "not_found", "No route matches "+r.Method+" "+r.URL.Path)
		return
	}

	state := &s.backupFreeze
	state.mu.Lock()
	if state.active {
		state.mu.Unlock()
		writeError(w, http.StatusConflict, "backup_freeze_active", "a backup freeze is already active")
		return
	}
	state.active = true
	state.mu.Unlock()

	// Bound the gate wait the same way the generic gate middleware does
	// (beginGateWorkBounded): without this, a raw, unbounded request context
	// would let this call queue indefinitely behind a long-running holder
	// instead of failing fast with the gate-busy response.
	gateCtx, cancel := context.WithTimeout(r.Context(), operationGateWaitLimit)
	defer cancel()
	done, ok := s.beginLabeledOperationGateWork(gateCtx, "backup freeze")
	if !ok {
		state.mu.Lock()
		state.active = false
		state.mu.Unlock()
		writeOperationGateBusy(w, s.operationGate)
		return
	}

	token := pack.NewPackID()
	state.mu.Lock()
	state.token = token
	state.release = done
	state.watchdog = time.AfterFunc(backupFreezeWatchdogTimeout, func() {
		s.releaseBackupFreezeOnWatchdog(token)
	})
	state.mu.Unlock()

	writeJSON(w, http.StatusOK, backupFreezeBeginResponse{Token: token})
}

// handleBackupFreezeEnd closes a backup freeze window opened by Begin,
// releasing the operation gate. An unknown or expired token (e.g. one whose
// window the watchdog already auto-released) is rejected: the caller's
// backup must fail rather than silently proceed unfrozen.
func (s *Server) handleBackupFreezeEnd(w http.ResponseWriter, r *http.Request) {
	if !isSameHostRequest(r) || !s.apiRequestAuthorized(r) {
		writeError(w, http.StatusNotFound, "not_found", "No route matches "+r.Method+" "+r.URL.Path)
		return
	}

	var req backupFreezeEndRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON request body")
		return
	}

	release, ok := s.clearBackupFreeze(req.Token)
	if !ok {
		writeError(w, http.StatusBadRequest, "backup_freeze_not_active",
			"no active backup freeze with that token")
		return
	}
	if release != nil {
		release()
	}
	writeJSON(w, http.StatusOK, backupFreezeEndResponse{})
}

// releaseBackupFreezeOnWatchdog fires when a backup freeze outlives
// backupFreezeWatchdogTimeout without a matching End call — e.g. because the
// backup subprocess crashed. It releases the operation gate so the daemon
// does not stay wedged, and logs at error level for operator visibility.
func (s *Server) releaseBackupFreezeOnWatchdog(token string) {
	release, ok := s.clearBackupFreeze(token)
	if !ok {
		// End already ran (or a prior watchdog fire already handled this
		// token); nothing to release.
		return
	}
	if release != nil {
		release()
	}
	s.logger.Error("backup freeze watchdog fired; releasing operation gate", "token", token)
}

// clearBackupFreeze clears freeze state for token, if it is the active
// freeze's token, stopping the watchdog and returning the gate release
// function to invoke. Both End and the watchdog call this, so whichever
// runs first wins the race and the other observes no active freeze.
func (s *Server) clearBackupFreeze(token string) (func(), bool) {
	state := &s.backupFreeze
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.active || token == "" || token != state.token {
		return nil, false
	}
	if state.watchdog != nil {
		state.watchdog.Stop()
	}
	release := state.release
	state.active = false
	state.token = ""
	state.release = nil
	state.watchdog = nil
	return release, true
}
