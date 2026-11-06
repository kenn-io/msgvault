package mcp

import (
	"net/http"
	"sync"
	"sync/atomic"
)

// httpState holds continuations that outlive a refreshed HTTP handler. The
// last handler sharing it releases it.
type httpState struct {
	mu            sync.Mutex
	refs          int
	downloads     *downloadCache
	confirmations *confirmationChallenges
}

func (s *httpState) retain() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs == 0 {
		return false
	}
	s.refs++
	return true
}

func (s *httpState) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs--; s.refs == 0 {
		s.downloads.close()
		s.confirmations.close()
	}
}

type httpHandler struct {
	http.Handler

	state  atomic.Pointer[httpState]
	closed atomic.Bool
}

// Inherit keeps previous's downloads and confirmation challenges for this
// handler. Call it before this handler serves and before previous closes.
func (h *httpHandler) Inherit(previous http.Handler) {
	old, ok := previous.(*httpHandler)
	if !ok || old == h {
		return
	}
	if state := old.state.Load(); state.retain() {
		h.state.Swap(state).release()
	}
}

func (h *httpHandler) withState(opts ServeOptions) (ServeOptions, *confirmationChallenges) {
	state := h.state.Load()
	opts.downloads = state.downloads
	return opts, state.confirmations
}

func (h *httpHandler) close() {
	if h.closed.CompareAndSwap(false, true) {
		h.state.Load().release()
	}
}
