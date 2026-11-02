package tiktokwebhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
)

var ErrSpoolFull = errors.New("TikTok webhook spool is full")

type Delivery struct {
	ID          string    `json:"id"`
	App         string    `json:"app"`
	Body        []byte    `json:"body"`
	ReceivedAt  time.Time `json:"received_at"`
	State       string    `json:"state"`
	Outcome     string    `json:"outcome,omitempty"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"next_attempt"`
}
type Spool struct {
	root    string
	maximum int64
	now     func() time.Time
}

func OpenSpool(root string, maximum int64) (*Spool, error) {
	if maximum <= 0 {
		return nil, errors.New("invalid TikTok spool capacity")
	}
	if err := fileutil.SecureMkdirAll(root, 0700); err != nil {
		return nil, err
	}
	// Persist every created directory entry before accepting a delivery.
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for parent := abs; ; parent = filepath.Dir(parent) {
		if err := syncDir(parent); err != nil {
			return nil, err
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	return &Spool{root: root, maximum: maximum, now: time.Now}, nil
}
func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	return errors.Join(err, f.Close())
}
func (s *Spool) lock(ctx context.Context, fn func() error) (err error) {
	lock := flock.New(filepath.Join(s.root, ".lock"), flock.SetPermissions(0600))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock TikTok spool: %w", err)
	}
	if !locked {
		return ctx.Err()
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	return fn()
}
func validDeliveryID(id string) bool {
	if len(id) != 64 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func (s *Spool) path(id string) string { return filepath.Join(s.root, id+".json") }
func (s *Spool) write(delivery Delivery) error {
	if !validDeliveryID(delivery.ID) {
		return errors.New("invalid TikTok delivery identifier")
	}
	data, err := json.Marshal(delivery)
	if err != nil {
		return err
	}
	return fileutil.SecureReplaceFile(s.path(delivery.ID), data, 0600)
}
func (s *Spool) scan() ([]Delivery, int64, error) {
	files, err := os.ReadDir(s.root)
	if err != nil {
		return nil, 0, err
	}
	var deliveries []Delivery
	var size int64
	for _, file := range files {
		id, ok := strings.CutSuffix(file.Name(), ".json")
		if !ok || !validDeliveryID(id) || file.IsDir() {
			continue
		}
		data, err := os.ReadFile(s.path(id))
		if err != nil {
			return nil, 0, err
		}
		var delivery Delivery
		if json.Unmarshal(data, &delivery) != nil || delivery.ID != id {
			return nil, 0, errors.New("invalid TikTok spool record")
		}
		deliveries = append(deliveries, delivery)
		size += int64(len(data))
	}
	slices.SortStableFunc(deliveries, func(a, b Delivery) int { return a.ReceivedAt.Compare(b.ReceivedAt) })
	return deliveries, size, nil
}
func (s *Spool) Accept(ctx context.Context, app string, body []byte) (id string, duplicate bool, err error) {
	sum := sha256.Sum256(append(append([]byte(app), 0), body...))
	id = hex.EncodeToString(sum[:])
	err = s.lock(ctx, func() error {
		if _, err := os.Stat(s.path(id)); err == nil {
			duplicate = true
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_, size, err := s.scan()
		if err != nil {
			return err
		}
		delivery := Delivery{ID: id, App: app, Body: body, ReceivedAt: s.now().UTC(), State: "pending"}
		data, err := json.Marshal(delivery)
		if err != nil {
			return err
		}
		// Leave settlement headroom for accepted records even at capacity.
		if size+int64(len(data))+1024 > s.maximum {
			return ErrSpoolFull
		}
		return s.write(delivery)
	})
	return id, duplicate, err
}

type Status struct {
	Pending       int       `json:"pending"`
	Held          int       `json:"held"`
	Failed        int       `json:"failed"`
	Done          int       `json:"done"`
	UnsupportedEU int       `json:"unsupported_eu"`
	Bytes         int64     `json:"bytes"`
	LastAccepted  time.Time `json:"last_accepted"`
}

func (s *Spool) Status(ctx context.Context, app string) (status Status, err error) {
	err = s.lock(ctx, func() error {
		deliveries, _, err := s.scan()
		if err != nil {
			return err
		}
		for _, d := range deliveries {
			if app != "" && d.App != app {
				continue
			}
			info, err := os.Stat(s.path(d.ID))
			if err != nil {
				return err
			}
			status.Bytes += info.Size()
			if d.ReceivedAt.After(status.LastAccepted) {
				status.LastAccepted = d.ReceivedAt
			}
			switch d.State {
			case "pending":
				status.Pending++
			case "held":
				status.Held++
			case "failed":
				status.Failed++
			case "done":
				status.Done++
			}
			if d.Outcome == "unsupported_eu" {
				status.UnsupportedEU++
			}
		}
		return nil
	})
	return status, err
}
func (s *Spool) Replay(ctx context.Context, app string) (count int, err error) {
	err = s.lock(ctx, func() error {
		deliveries, _, err := s.scan()
		if err != nil {
			return err
		}
		for _, d := range deliveries {
			if (app == "" || d.App == app) && (d.State == "held" || d.State == "failed") {
				d.State, d.Outcome, d.Attempts, d.NextAttempt = "pending", "", 0, time.Time{}
				if err := s.write(d); err != nil {
					return err
				}
				count++
			}
		}
		return nil
	})
	return count, err
}

type Outcome struct{ Class string }
type HandlerFunc func(context.Context, string, []byte) (Outcome, error)

// Drain is single-consumer. Delivery IO is locked across processes; archive
// work runs outside the spool lock so HTTP acceptance can continue.
func (s *Spool) Drain(ctx context.Context, handle HandlerFunc) (applied int, err error) {
	var due []Delivery
	err = s.lock(ctx, func() error {
		deliveries, _, err := s.scan()
		if err != nil {
			return err
		}
		for _, d := range deliveries {
			if d.State == "pending" && !d.NextAttempt.After(s.now()) {
				due = append(due, d)
				if len(due) == 100 {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return applied, err
	}
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		outcome, processErr := handle(ctx, d.App, d.Body)
		if processErr != nil {
			d.Attempts++
			d.Outcome = "processing_failed"
			d.NextAttempt = s.now().Add(time.Duration(1<<min(d.Attempts, 8)) * time.Second)
			if d.Attempts >= 12 {
				d.State = "failed"
			}
		} else {
			d.Outcome = outcome.Class
			switch outcome.Class {
			case "applied":
				d.State = "done"
				applied++
			case "unsupported_eu":
				d.State = "done"
			default:
				d.State = "held"
			}
		}
		// Settle even when archive work was interrupted; raw bytes remain
		// recoverable if shutdown prevents publishing the new state.
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		settleErr := s.lock(settleCtx, func() error { return s.write(d) })
		cancel()
		if settleErr != nil {
			return applied, settleErr
		}
	}
	return applied, nil
}

func (s *Spool) Prune(ctx context.Context, retention time.Duration) error {
	return s.lock(ctx, func() error {
		deliveries, _, err := s.scan()
		if err != nil {
			return err
		}
		for _, d := range deliveries {
			if d.State == "done" && s.now().Sub(d.ReceivedAt) > retention {
				if err := os.Remove(s.path(d.ID)); err != nil {
					return err
				}
			}
		}
		return syncDir(s.root)
	})
}
