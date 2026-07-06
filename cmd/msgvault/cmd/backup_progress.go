package cmd

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/backup"
)

// backupProgressTickInterval throttles how often backupProgressRenderer
// redraws its in-place status line (TTY mode) or emits a new line (plain
// mode). This throttling used to live inside go.kenn.io/kit/backup; it now
// lives here because only the renderer — not the library — knows whether its
// output is a live terminal or a pipe (the daemon CLI subprocess, a
// redirect, CI). It is a var, not a const, so tests can shorten it. Final
// events always render immediately, regardless of this interval, since they
// mark a stage boundary the renderer must not miss or delay.
var backupProgressTickInterval = 2 * time.Second

// backupProgressBarStyle draws the backup progress bar with solid/light
// unicode block glyphs, reusing formatCLIProgressBar's existing bar-drawing
// logic from progress_bar.go (the repo's established percent-bar helper;
// only the fill glyphs differ from its ASCII default).
var backupProgressBarStyle = cliProgressBarStyle{
	Width:  cliProgressBarWidth,
	Filled: "█",
	Empty:  "░",
}

// backupProgressStageLabels names each backup.ProgressStage for display.
var backupProgressStageLabels = map[backup.ProgressStage]string{
	backup.ProgressStageFreeze:      "freeze",
	backup.ProgressStageScan:        "scan",
	backup.ProgressStagePack:        "pack",
	backup.ProgressStageAttachments: "attachments",
	backup.ProgressStageSeal:        "seal",
	backup.ProgressStageVerify:      "verify",
	backup.ProgressStageRestoreDB:   "db",
	backup.ProgressStageExtras:      "extras",
	backup.ProgressStageProof:       "proof",
}

// backupProgressStageLabel returns stage's display label, falling back to
// its raw string value for any stage this file doesn't know about (so a
// future stage still renders instead of showing a blank label).
func backupProgressStageLabel(stage backup.ProgressStage) string {
	if label, ok := backupProgressStageLabels[stage]; ok {
		return label
	}
	return string(stage)
}

// backupProgressRenderer turns a backup.ProgressEvent stream (from
// backup.Create or backup.Verify) into either a single, in-place-redrawn
// status line per stage (TTY) or throttled, newline-terminated lines
// (plain — stdout is a pipe: the daemon CLI subprocess, redirected output,
// CI, where \r cannot overwrite and would interleave unreadably). It follows
// the same auto/tty/plain approach CLIProgress (syncfull.go) already
// established for sync-full, reusing its progressOutputMode type.
type backupProgressRenderer struct {
	mu         sync.Mutex
	mode       progressOutputMode
	out        io.Writer
	stage      backup.ProgressStage
	stageStart time.Time
	lastRender time.Time
	stageOpen  bool // true once the current stage has printed an in-place (non-final) TTY line
	lastWidth  int  // rune width of the last in-place TTY line, for pad-clearing shorter redraws
	lastEvent  backup.ProgressEvent
	tickerStop chan struct{}
}

// backupProgressElapsedTick is how often the idle ticker re-renders an open
// TTY line so its elapsed time keeps counting while the underlying stage is
// silent (integrity_check reads a whole database without emitting events).
// A var so tests can shorten it.
var backupProgressElapsedTick = time.Second

// newBackupProgressRenderer builds a renderer writing to out (nil defaults
// to os.Stdout) in mode. progressModeAuto detects the mode from out's own
// terminal-ness the first time an event is handled.
func newBackupProgressRenderer(out io.Writer, mode progressOutputMode) *backupProgressRenderer {
	return &backupProgressRenderer{mode: mode, out: out}
}

func (r *backupProgressRenderer) writer() io.Writer {
	if r.out == nil {
		return os.Stdout
	}
	return r.out
}

func (r *backupProgressRenderer) outputMode() progressOutputMode {
	if r.mode == progressModeAuto {
		if f, ok := r.writer().(*os.File); ok &&
			(isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())) {
			r.mode = progressModeTTY
		} else {
			r.mode = progressModePlain
		}
	}
	return r.mode
}

// handle renders one backup.ProgressEvent. It is meant to be passed
// directly as backup.CreateOptions.Progress / backup.VerifyOptions.Progress.
func (r *backupProgressRenderer) handle(ev backup.ProgressEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.Stage != r.stage {
		if r.stageOpen {
			// Defensive: a new stage started without a preceding Final event
			// for the last one (shouldn't happen given go.kenn.io/kit/backup's
			// contract, but this keeps a stray in-place line from being
			// overwritten by the next stage's redraw).
			_, _ = fmt.Fprintln(r.writer())
			r.stageOpen = false
			r.lastWidth = 0
		}
		r.stage = ev.Stage
		r.stageStart = time.Now()
		r.lastRender = time.Time{}
	}
	if !ev.Final {
		// Record the freshest counts even when the throttle suppresses this
		// redraw, so the idle ticker repaints current progress rather than
		// the last event that happened to render.
		r.lastEvent = ev
	}
	if !ev.Final && time.Since(r.lastRender) < backupProgressTickInterval {
		return
	}
	r.lastRender = time.Now()
	r.render(ev)
}

// render writes one status update for ev, in whichever output mode is
// active.
func (r *backupProgressRenderer) render(ev backup.ProgressEvent) {
	label := backupProgressStageLabel(ev.Stage)
	pct := backupProgressPercent(ev.Done, ev.Total)
	counts := fmt.Sprintf("%d/%d", ev.Done, ev.Total)
	if ev.Total == 0 {
		counts = strconv.FormatInt(ev.Done, 10)
	}

	extra := ""
	if ev.BytesDone > 0 {
		extra = " " + formatSize(ev.BytesDone)
		if rate := r.stageRate(ev); rate > 0 {
			extra += fmt.Sprintf(" @ %s/s", formatSize(int64(rate)))
		}
	}

	if r.outputMode() == progressModePlain {
		suffix := r.elapsedSuffix()
		if ev.Final {
			suffix += " (done)"
		}
		_, _ = fmt.Fprintf(r.writer(), "%s: %s (%3.0f%%)%s%s\n", label, counts, pct, extra, suffix)
		return
	}

	bar := formatCLIProgressBar(pct, backupProgressBarStyle)
	line := fmt.Sprintf("  %-11s %s %3.0f%%  %s%s%s", label, bar, pct, counts, extra, r.elapsedSuffix())
	// \r alone does not clear the row, so a redraw shorter than its
	// predecessor (a rate blip, a compact final line) would leave the old
	// tail visible. Pad to the widest line rendered so far this stage.
	width := utf8.RuneCountInString(line)
	if width < r.lastWidth {
		line += strings.Repeat(" ", r.lastWidth-width)
	} else {
		r.lastWidth = width
	}
	if ev.Final {
		_, _ = fmt.Fprintln(r.writer(), "\r"+line)
		r.stageOpen = false
		r.lastWidth = 0
	} else {
		_, _ = fmt.Fprint(r.writer(), "\r"+line)
		r.stageOpen = true
		r.startElapsedTickerLocked()
	}
}

// elapsedSuffix formats the current stage's wall-clock age for display,
// empty during a stage's first second so fast stages stay uncluttered.
func (r *backupProgressRenderer) elapsedSuffix() string {
	elapsed := time.Since(r.stageStart).Truncate(time.Second)
	if elapsed < time.Second {
		return ""
	}
	return " " + elapsed.String()
}

// startElapsedTickerLocked lazily starts the idle re-render loop. Callers
// must hold r.mu. The ticker re-renders the last in-place line whenever no
// real event has arrived for a full throttle interval, so elapsed time keeps
// counting during long silent stages.
func (r *backupProgressRenderer) startElapsedTickerLocked() {
	if r.tickerStop != nil {
		return
	}
	stop := make(chan struct{})
	r.tickerStop = stop
	go func() {
		ticker := time.NewTicker(backupProgressElapsedTick)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				r.mu.Lock()
				if r.stageOpen && time.Since(r.lastRender) >= backupProgressTickInterval {
					r.render(r.lastEvent)
				}
				r.mu.Unlock()
			}
		}
	}()
}

// finish closes any in-place TTY line still open so subsequent output (error
// messages, summaries) starts on a fresh row. It is safe to call when nothing
// is open and after normal completion; defer it right after constructing a
// renderer.
func (r *backupProgressRenderer) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tickerStop != nil {
		close(r.tickerStop)
		r.tickerStop = nil
	}
	if r.stageOpen {
		_, _ = fmt.Fprintln(r.writer())
		r.stageOpen = false
		r.lastWidth = 0
	}
}

// stageRate estimates ev.BytesDone's throughput over the current stage's
// wall-clock duration so far. It returns 0 until the stage has run at least
// a second, since a rate computed over a shorter window is too noisy to be
// useful.
func (r *backupProgressRenderer) stageRate(ev backup.ProgressEvent) float64 {
	elapsed := time.Since(r.stageStart).Seconds()
	if elapsed < 1 {
		return 0
	}
	return float64(ev.BytesDone) / elapsed
}

// backupProgressPercent computes a 0-100 percentage, clamped, treating a
// non-positive total as "unknown" (0%).
func backupProgressPercent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	pct := float64(done) / float64(total) * 100
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	return pct
}

// backupProgressModeFromFlag maps the --progress flag's value to a
// progressOutputMode. "auto" (the default) defers mode detection to the
// renderer's own outputMode(), which checks its writer's terminal-ness.
func backupProgressModeFromFlag(value string) (progressOutputMode, error) {
	switch value {
	case "", "auto":
		return progressModeAuto, nil
	case "bar":
		return progressModeTTY, nil
	case "plain":
		return progressModePlain, nil
	default:
		return progressModeAuto, fmt.Errorf("backup: invalid --progress value %q (want auto, bar, or plain)", value)
	}
}

// resolveClientBackupProgressFlag resolves a "--progress auto" (the
// default, whether the user passed it explicitly or left it at its zero
// value) to a concrete "bar" or "plain" using the CLIENT's own stdout
// terminal-ness, and writes that resolved value back onto cmd's flag.
//
// backup create proxies through the daemon: the daemon re-spawns this same
// command in a subprocess whose stdout is a pipe back to the daemon, never a
// real terminal, so that subprocess's own auto-detection would always
// resolve to "plain" even when the end user's actual terminal is
// interactive. Resolving here, on the client, and forwarding the concrete
// value avoids that.
func resolveClientBackupProgressFlag(cmd *cobra.Command) error {
	flag := cmd.Flags().Lookup("progress")
	if flag == nil {
		return nil
	}
	value := flag.Value.String()
	switch value {
	case "", "auto":
		resolved := "plain"
		if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
			resolved = "bar"
		}
		if err := cmd.Flags().Set("progress", resolved); err != nil {
			return fmt.Errorf("backup create: resolving --progress to %q: %w", resolved, err)
		}
		return nil
	case "bar", "plain":
		return nil
	default:
		return fmt.Errorf("backup create: invalid --progress value %q (want auto, bar, or plain)", value)
	}
}
