package metawebhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/atomicfile"
)

type State string

const (
	StatePending State = "pending"
	StateHeld    State = "held"
	StateFailed  State = "failed"
	StateDone    State = "done"
)

var states = []State{StatePending, StateHeld, StateFailed, StateDone}
var appPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
var ErrSpoolFull = errors.New("webhook spool is full")

type DeliveryMeta struct {
	NextItem       int            `json:"nextItem,omitempty"`
	ReceiptOrder   uint64         `json:"receiptOrder,omitempty"`
	ID             string         `json:"id"`
	AppID          string         `json:"appId"`
	State          State          `json:"state"`
	ReceivedAt     time.Time      `json:"receivedAt"`
	NextAttemptAt  time.Time      `json:"nextAttemptAt"`
	Attempts       int            `json:"attempts"`
	LastErrorClass string         `json:"lastErrorClass"`
	Outcomes       map[string]int `json:"outcomes"`
}
type Settlement struct {
	NextItem     int
	Yielded      bool
	State        State
	Outcomes     map[string]int
	ErrorClass   string
	CountAttempt bool
}
type RequeueFilter struct {
	AppID      string
	States     []State
	DeliveryID string
	Since      time.Time
}
type AppStats struct {
	Pending, Held, Failed, Done int
	OldestPending               time.Time
	Bytes                       int64
	HeldOutcomes                map[string]int
}

type Spool struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	now      func() time.Time
	bytes    int64
	bytesAt  time.Time
	order    uint64
}

func validID(id string) bool {
	if len(id) != 64 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func syncDir(path string) error {
	f, err := os.Open(path) // #nosec G703 -- path is a local spool directory selected by configuration or derived from validated IDs.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// Windows rejects FlushFileBuffers for directory handles. File contents
	// are still flushed before publication; directory fsync is Unix-only.
	if runtime.GOOS == "windows" {
		return nil
	}
	return f.Sync()
}
func mkdir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil { // #nosec G703 -- path is the configured local spool directory, never an HTTP request path.
		return err
	}
	return os.Chmod(path, 0700) // #nosec G302 G703 -- private directories need owner execute permission; path is local configuration or derived from validated IDs.
}

// atomicWrite leaves a complete file or a removable temporary file after a crash.
func atomicWrite(path string, data []byte) error {
	if err := mkdir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }() // #nosec G703 -- name is the exact path returned by os.CreateTemp in the local spool directory.
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = atomicfile.Replace(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}
func readJSON(path string, v any) error {
	f, err := openSpoolRead(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Root.Open permits Windows atomic replacement while a reader holds the file.
func openSpoolRead(path string) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.Open(filepath.Base(path))
}
func (s *Spool) path(app string, state State, id string) string {
	return filepath.Join(s.dir, app, string(state), id+".json")
}
func statePath(body string) string { return strings.TrimSuffix(body, ".json") + ".state.json" }
func (s *Spool) layout(app string) error {
	for _, state := range append([]State{"tmp"}, states...) {
		if err := mkdir(filepath.Join(s.dir, app, string(state))); err != nil {
			return err
		}
	}
	if err := syncDir(filepath.Join(s.dir, app)); err != nil {
		return err
	}
	return syncDir(s.dir)
}
func OpenSpool(dir string, maxBytes int64, now func() time.Time) (*Spool, error) {
	if maxBytes <= 0 {
		return nil, errors.New("invalid spool capacity")
	}
	if now == nil {
		now = time.Now
	}
	if err := mkdir(dir); err != nil {
		return nil, err
	}
	// MkdirAll may have created several directory entries. Persist their
	// parents at startup before any delivery can be acknowledged.
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for parent := filepath.Dir(root); ; parent = filepath.Dir(parent) {
		if err := syncDir(parent); err != nil {
			return nil, err
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	if err := mkdir(filepath.Join(dir, "requests")); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, maxBytes: maxBytes, now: now}
	if err := readJSON(filepath.Join(dir, "receipt-order.json"), &s.order); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read receipt order: %w", err)
	}
	apps, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, app := range apps {
		if !app.IsDir() || !appPattern.MatchString(app.Name()) {
			continue
		}
		if err = s.layout(app.Name()); err != nil {
			return nil, err
		}
		leftovers, e := os.ReadDir(filepath.Join(dir, app.Name(), "tmp"))
		if e != nil {
			return nil, e
		}
		for _, file := range leftovers {
			if e = os.RemoveAll(filepath.Join(dir, app.Name(), "tmp", file.Name())); e != nil {
				return nil, e
			}
		}
		if err = s.recoverMovedMetadata(app.Name()); err != nil {
			return nil, err
		}
		for _, state := range states {
			folder := filepath.Join(dir, app.Name(), string(state))
			files, e := os.ReadDir(folder)
			if e != nil {
				return nil, e
			}
			for _, file := range files {
				path := filepath.Join(folder, file.Name())
				if strings.HasPrefix(file.Name(), ".tmp-") {
					if e = os.Remove(path); e != nil {
						return nil, e
					}
					continue
				}
				if strings.HasSuffix(file.Name(), ".state.json") {
					body := strings.TrimSuffix(path, ".state.json") + ".json"
					if _, e = os.Stat(body); errors.Is(e, fs.ErrNotExist) {
						if e = os.Remove(path); e != nil {
							return nil, e
						}
					} else if e != nil {
						return nil, e
					}
					continue
				}
				id := strings.TrimSuffix(file.Name(), ".json")
				if !validID(id) {
					continue
				}
				if _, e = os.Stat(statePath(path)); errors.Is(e, fs.ErrNotExist) {
					order, err := s.reserveOrder()
					if err != nil {
						return nil, err
					}
					meta := DeliveryMeta{ID: id, AppID: app.Name(), State: StatePending, ReceivedAt: now(), ReceiptOrder: order}
					target := s.path(app.Name(), StatePending, id)
					if path != target {
						if e = atomicfile.Replace(path, target); e != nil {
							return nil, e
						}
						if e = syncDir(folder); e != nil {
							return nil, e
						}
					}
					if e = writeJSON(statePath(target), meta); e != nil {
						return nil, e
					}
				} else if e != nil {
					return nil, e
				}
			}
		}
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	return s, nil
}

// Recover interrupted body/sidecar moves before orphan cleanup. A sidecar in
// another state still owns the receipt order; an uncertain transition is safe
// to retry using that same order, never as a newly received event.
func (s *Spool) recoverMovedMetadata(app string) error {
	for _, state := range states {
		files, err := os.ReadDir(filepath.Join(s.dir, app, string(state)))
		if err != nil {
			return err
		}
		for _, file := range files {
			id := strings.TrimSuffix(file.Name(), ".json")
			if !validID(id) {
				continue
			}
			body := s.path(app, state, id)
			if _, err := os.Stat(statePath(body)); err == nil {
				continue
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			for _, oldState := range states {
				var meta DeliveryMeta
				err := readJSON(statePath(s.path(app, oldState, id)), &meta)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				meta.ID, meta.AppID, meta.State = id, app, StatePending
				if err := s.move(body, meta); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// Reserve a disjoint range for each delivery before publishing it. The counter
// survives pruning; retries retain their range and explicit replay gets a new one.
func (s *Spool) reserveOrder() (uint64, error) {
	if s.order > math.MaxUint64-maxDeliveryItems {
		return 0, errors.New("receipt order exhausted")
	}
	first := s.order + 1
	// Advance memory even if persistence fails ambiguously, so this process
	// cannot reuse an order whose file replacement may already have succeeded.
	s.order += maxDeliveryItems
	if err := writeJSON(filepath.Join(s.dir, "receipt-order.json"), s.order); err != nil {
		return 0, fmt.Errorf("persist receipt order: %w", err)
	}
	return first, nil
}

func (s *Spool) locate(app, id string) (string, error) {
	if !appPattern.MatchString(app) || !validID(id) {
		return "", errors.New("invalid delivery identity")
	}
	for _, state := range states {
		path := s.path(app, state, id)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fs.ErrNotExist
}
func (s *Spool) Accept(app string, body []byte) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !appPattern.MatchString(app) {
		return "", false, errors.New("invalid app ID")
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(app + "\x00"))
	_, _ = hash.Write(body)
	id := hex.EncodeToString(hash.Sum(nil))
	if path, err := s.locate(app, id); err == nil {
		if _, err = os.Stat(statePath(path)); errors.Is(err, fs.ErrNotExist) { // #nosec G703 -- locate derives path from a validated app ID and a computed SHA-256 delivery ID.
			order, err := s.reserveOrder()
			if err != nil {
				return "", false, err
			}
			meta := DeliveryMeta{ID: id, AppID: app, State: StatePending, ReceivedAt: s.now(), ReceiptOrder: order}
			if err = s.move(path, meta); err != nil {
				return "", false, err
			}
		} else if err != nil {
			return "", false, err
		}
		// A previous Accept may have linked the body and then failed before fsync.
		if err = syncDir(filepath.Dir(path)); err != nil {
			return "", false, err
		}
		return id, true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	if s.bytesAt.IsZero() || s.now().Sub(s.bytesAt) >= 10*time.Second {
		stats, err := ReadStats(s.dir)
		if err != nil {
			return "", false, err
		}
		s.bytes = 0
		for _, a := range stats {
			s.bytes += a.Bytes
		}
		s.bytesAt = s.now()
	}
	if s.order > math.MaxUint64-maxDeliveryItems {
		return "", false, errors.New("receipt order exhausted")
	}
	meta := DeliveryMeta{ID: id, AppID: app, State: StatePending, ReceivedAt: s.now(), ReceiptOrder: s.order + 1}
	data, err := json.Marshal(meta)
	if err != nil {
		return "", false, err
	}
	if s.bytes+int64(len(body)+len(data)) > s.maxBytes {
		return "", false, ErrSpoolFull
	}
	if err = s.layout(app); err != nil {
		return "", false, err
	}
	if _, err := s.reserveOrder(); err != nil {
		return "", false, err
	}
	s.bytesAt = time.Time{} // Force a rescan if any subsequent write fails.
	f, err := os.CreateTemp(filepath.Join(s.dir, app, "tmp"), "delivery-")
	if err != nil {
		return "", false, err
	}
	temp := f.Name()
	defer func() { _ = os.Remove(temp) }() // #nosec G703 -- temp is the exact path returned by os.CreateTemp in the local spool directory.
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", false, err
	}
	if closeErr != nil {
		return "", false, closeErr
	}
	target := s.path(app, StatePending, id)
	if err = os.Link(temp, target); errors.Is(err, fs.ErrExist) {
		return id, true, nil
	} else if err != nil {
		return "", false, err
	}
	if err = atomicWrite(statePath(target), data); err != nil {
		return "", false, err
	}
	if err = syncDir(filepath.Dir(target)); err != nil {
		return "", false, err
	}
	s.bytes += int64(len(body) + len(data))
	s.bytesAt = s.now()
	return id, false, nil
}
func (s *Spool) Body(app, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.locate(app, id)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func walkDeliveries(root string, fn func(string, DeliveryMeta) error) error {
	return walkDeliveriesIgnoring(root, fn, func(error) bool { return false })
}

func walkDeliveriesIgnoring(root string, fn func(string, DeliveryMeta) error, ignoreReadError func(error) bool) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		id := strings.TrimSuffix(d.Name(), ".json")
		if !validID(id) {
			return nil
		}
		state := State(filepath.Base(filepath.Dir(path)))
		app := filepath.Base(filepath.Dir(filepath.Dir(path)))
		if !slices.Contains(states, state) || !appPattern.MatchString(app) {
			return nil
		}
		var meta DeliveryMeta
		if err = readJSON(statePath(path), &meta); errors.Is(err, fs.ErrNotExist) {
			info, e := d.Info()
			if errors.Is(e, fs.ErrNotExist) || ignoreReadError(e) {
				return nil
			}
			if e != nil {
				return fmt.Errorf("inspect spool delivery: %w", e)
			}
			meta = DeliveryMeta{ID: id, AppID: app, State: StatePending, ReceivedAt: info.ModTime()}
		} else if err != nil {
			if ignoreReadError(err) {
				return nil
			}
			return err
		}
		meta.ID, meta.AppID = id, app
		return fn(path, meta)
	})
}
func (s *Spool) Due(limit int) ([]DeliveryMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DeliveryMeta
	err := walkDeliveries(s.dir, func(_ string, m DeliveryMeta) error {
		if m.State == StatePending && !m.NextAttemptAt.After(s.now()) {
			out = append(out, m)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReceiptOrder != out[j].ReceiptOrder {
			return out[i].ReceiptOrder < out[j].ReceiptOrder
		}
		if out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].ReceivedAt.Before(out[j].ReceivedAt)
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}
func (s *Spool) move(path string, meta DeliveryMeta) error {
	s.bytesAt = time.Time{}
	target := s.path(meta.AppID, meta.State, meta.ID)
	// Publish the new metadata before moving either name. In particular an
	// explicit replay's new receipt order survives either crash boundary.
	if err := writeJSON(statePath(path), meta); err != nil {
		return err
	}
	if path != target {
		if err := atomicfile.Replace(path, target); err != nil {
			return err
		}
		if err := atomicfile.Replace(statePath(path), statePath(target)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(target))
}
func (s *Spool) Settle(app, id string, st Settlement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(states, st.State) {
		return errors.New("invalid delivery state")
	}
	path, err := s.locate(app, id)
	if err != nil {
		return err
	}
	var m DeliveryMeta
	if err = readJSON(statePath(path), &m); err != nil {
		return err
	}
	m.NextItem = st.NextItem
	m.State, m.Outcomes, m.LastErrorClass = st.State, st.Outcomes, st.ErrorClass
	m.NextAttemptAt = time.Time{}
	if st.CountAttempt {
		m.Attempts++
	}
	if m.State == StatePending {
		if m.Attempts >= 12 {
			m.State = StateFailed
		} else if !st.Yielded {
			delay := 30 * time.Second
			for n := 1; n < m.Attempts; n++ {
				delay = min(delay*2, time.Hour)
			}
			m.NextAttemptAt = s.now().Add(delay)
		}
	}
	return s.move(path, m)
}
func (s *Spool) Requeue(f RequeueFilter, dry bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	err := walkDeliveries(s.dir, func(path string, m DeliveryMeta) error {
		if (f.AppID != "" && f.AppID != m.AppID) || (f.DeliveryID != "" && f.DeliveryID != m.ID) || (!f.Since.IsZero() && m.ReceivedAt.Before(f.Since)) || (len(f.States) > 0 && !slices.Contains(f.States, m.State)) {
			return nil
		}
		if m.State == StatePending {
			return nil
		}
		n++
		if dry {
			return nil
		}
		order, err := s.reserveOrder()
		if err != nil {
			return err
		}
		m.ReceiptOrder, m.NextItem = order, 0
		m.State, m.Attempts, m.NextAttemptAt, m.LastErrorClass, m.Outcomes = StatePending, 0, time.Time{}, "", nil
		return s.move(path, m)
	})
	return n, err
}
func (s *Spool) Prune(retention time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytesAt = time.Time{}
	n := 0
	err := walkDeliveries(s.dir, func(path string, m DeliveryMeta) error {
		if m.State != StateDone || !m.ReceivedAt.Before(s.now().Add(-retention)) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.Remove(statePath(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		n++
		return syncDir(filepath.Dir(path))
	})
	return n, err
}
func (s *Spool) Stats() (map[string]AppStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ReadStats(s.dir)
}
func ReadStats(root string) (map[string]AppStats, error) {
	out := map[string]AppStats{}
	err := walkDeliveriesIgnoring(root, func(_ string, m DeliveryMeta) error {
		a := out[m.AppID]
		switch m.State {
		case StatePending:
			a.Pending++
			if a.OldestPending.IsZero() || m.ReceivedAt.Before(a.OldestPending) {
				a.OldestPending = m.ReceivedAt
			}
		case StateHeld:
			a.Held++
		case StateFailed:
			a.Failed++
		case StateDone:
			a.Done++
		}
		if a.HeldOutcomes == nil {
			a.HeldOutcomes = map[string]int{}
		}
		if m.State == StateHeld {
			for k, n := range m.Outcomes {
				a.HeldOutcomes[k] += n
			}
		}
		out[m.AppID] = a
		return nil
	}, transientSpoolReadError)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 || !appPattern.MatchString(parts[0]) {
			return nil
		}
		info, e := d.Info()
		if errors.Is(e, fs.ErrNotExist) || transientSpoolReadError(e) {
			return nil
		}
		if e != nil {
			return fmt.Errorf("inspect spool file: %w", e)
		}
		a := out[parts[0]]
		a.Bytes += info.Size()
		out[parts[0]] = a
		return nil
	})
	return out, err
}
