package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	logsFollow bool
	logsLines  int
	logsRunID  string
	logsLevel  string
	logsAll    bool
	logsGrep   string
	logsPath   bool
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View and tail msgvault's structured log files",
	Long: `Show msgvault's structured log output from the on-disk JSON logs
written to <data dir>/logs.

By default this prints the last 50 lines of today's log file in a
compact, human-friendly format (level + run_id + message + the
interesting attrs). Use --follow to tail the file live, --run-id
to filter to a single invocation, --level to filter by severity,
or --grep to filter on a substring match across the whole record.

Examples:

  msgvault logs                       # last 50 lines of today's log
  msgvault logs -n 200 --follow       # tail with --follow
  msgvault logs --run-id a1b2c3d4     # just one run
  msgvault logs --level error         # only errors
  msgvault logs --grep deduplicate    # substring over the JSON
  msgvault logs --all                 # every log file we still have
  msgvault logs --path                # print the log path and exit`,
	RunE: runLogsCmd,
}

func runLogsCmd(cmd *cobra.Command, args []string) error {
	dir := cfg.LogsDir()

	if logsPath {
		fmt.Println(dir)
		return nil
	}

	files, err := findLogFiles(dir, logsAll)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Printf("No log files in %s\n", dir)
		fmt.Println("(Nothing has been logged yet, or --no-log-file was used.)")
		return nil
	}

	filter := logFilter{
		RunID: logsRunID,
		Level: strings.ToLower(logsLevel),
		Grep:  logsGrep,
	}

	// Non-follow mode: load the requested file(s) and print the
	// last N filtered lines. "Last N" is computed against the
	// filtered subset so --run-id and --level behave intuitively.
	if !logsFollow {
		return printLogFiles(files, logsLines, filter, cmd.OutOrStdout())
	}

	// Follow mode: print the tail of the most recent file and
	// then stream new lines. --all is ignored because tailing
	// rotated files would be a trap.
	latest := files[len(files)-1]
	if err := printLogFiles(
		[]string{latest}, logsLines, filter, cmd.OutOrStdout(),
	); err != nil {
		return err
	}
	return followLogFile(cmd.Context(), latest, filter, cmd.OutOrStdout())
}

// findLogFiles returns the sorted list of log files to read.
// When all is false, it returns only today's file (if it exists).
func findLogFiles(dir string, all bool) ([]string, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat logs dir: %w", err)
	}

	if !all {
		name := fmt.Sprintf(
			"msgvault-%s.log", time.Now().UTC().Format("2006-01-02"),
		)
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return []string{path}, nil
		}
		// Fall through to the --all scan; maybe we only have
		// yesterday's file.
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read logs dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, "msgvault-") ||
			(!strings.HasSuffix(n, ".log") && !strings.Contains(n, ".log.")) {
			continue
		}
		files = append(files, filepath.Join(dir, n))
	}
	sort.Slice(files, func(i, j int) bool {
		return logFileSortKey(files[i]) < logFileSortKey(files[j])
	})
	return files, nil
}

// logFileSortKey returns a string that sorts log files chronologically:
// rotated files (.log.1, .log.2) come before the active .log for the
// same date. Higher rotation indices are older (.log.5 predates .log.1),
// so they sort first by inverting the suffix.
func logFileSortKey(path string) string {
	name := filepath.Base(path)
	// msgvault-2026-04-11.log   -> date=2026-04-11 suffix=999 (active, last)
	// msgvault-2026-04-11.log.5 -> date=2026-04-11 suffix=000 (oldest rotation)
	// msgvault-2026-04-11.log.1 -> date=2026-04-11 suffix=004 (newest rotation)
	if idx := strings.LastIndex(name, ".log."); idx >= 0 {
		date := name[:idx+4] // through ".log"
		num := 0
		_, _ = fmt.Sscanf(name[idx+5:], "%d", &num)
		// Invert: higher rotation number = older = should sort first.
		// 999 is reserved for the active file, so cap at 998.
		inverted := max(998-num, 0)
		return fmt.Sprintf("%s.%03d", date, inverted)
	}
	// Active file (no rotation suffix) sorts after all rotations
	// for the same date.
	return name + ".999"
}

// logFilter represents the user's --run-id / --level / --grep
// filters. An empty field means "no filter on that axis".
type logFilter struct {
	RunID string
	Level string
	Grep  string
}

// matches reports whether a record matches every active filter.
func (f logFilter) matches(raw []byte, rec map[string]any) bool {
	if f.RunID != "" {
		if got, _ := rec["run_id"].(string); !strings.HasPrefix(got, f.RunID) {
			return false
		}
	}
	if f.Level != "" {
		if got, _ := rec["level"].(string); !strings.EqualFold(got, f.Level) {
			return false
		}
	}
	if f.Grep != "" {
		if !strings.Contains(string(raw), f.Grep) {
			return false
		}
	}
	return true
}

// printLogFiles prints the last tailN filtered lines across the
// supplied files. Keeping a fixed-size ring buffer keeps memory
// bounded even on very large log files.
func printLogFiles(
	files []string, tailN int, filter logFilter, out io.Writer,
) error {
	if tailN <= 0 {
		tailN = 50
	}
	ring := make([]string, 0, tailN)
	push := func(line string) {
		if len(ring) == tailN {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}

	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 4*1024*1024)
		for scanner.Scan() {
			raw := scanner.Bytes()
			var rec map[string]any
			if err := json.Unmarshal(raw, &rec); err != nil {
				// Non-JSON lines (shouldn't happen in file
				// output, but be safe).
				continue
			}
			if !filter.matches(raw, rec) {
				continue
			}
			push(formatLogRecord(rec))
		}
		_ = f.Close()
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("scan %s: %w", path, err)
		}
	}
	for _, line := range ring {
		_, _ = fmt.Fprintln(out, line)
	}
	return nil
}

// followLogFile tails path for new lines as they're written and
// prints those that match filter. Exits when the command context
// is cancelled (Ctrl-C).
func followLogFile(
	ctx context.Context, path string, filter logFilter, out io.Writer,
) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek end: %w", err)
	}

	reader := bufio.NewReader(f)
	var partial []byte
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if len(partial) > 0 {
				line = append(partial, line...)
				partial = nil
			}
			// If the line doesn't end with a newline, it's a
			// partial read — buffer it until more data arrives.
			if line[len(line)-1] != '\n' {
				partial = append(partial[:0], line...)
				// fall through to the sleep
			} else {
				var rec map[string]any
				if json.Unmarshal(line, &rec) == nil &&
					filter.matches(line, rec) {
					_, _ = fmt.Fprintln(out, formatLogRecord(rec))
				}
				continue
			}
		}
		if err != nil && err != io.EOF {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// formatLogRecord renders a JSON log record as a compact, human
// readable line. The interesting attributes come after the
// message; we deliberately drop the source attribute for brevity.
func formatLogRecord(rec map[string]any) string {
	level, _ := rec["level"].(string)
	msg, _ := rec["msg"].(string)
	runID, _ := rec["run_id"].(string)
	ts, _ := rec["time"].(string)

	// Collect the remaining interesting attributes in a stable
	// order. Known low-signal keys are skipped.
	skip := map[string]bool{
		"level": true, "msg": true, "run_id": true,
		"time": true, "source": true,
	}
	keys := make([]string, 0, len(rec))
	for k := range rec {
		if !skip[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var b strings.Builder
	if ts != "" {
		// Keep just HH:MM:SS for readability — the file name
		// already encodes the date.
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			b.WriteString(t.Local().Format("15:04:05"))
			b.WriteByte(' ')
		}
	}
	if level != "" {
		fmt.Fprintf(&b, "%-5s", level)
		b.WriteByte(' ')
	}
	if runID != "" {
		// Show first 6 chars so the column stays aligned.
		short := runID
		if len(short) > 6 {
			short = short[:6]
		}
		b.WriteString(short)
		b.WriteByte(' ')
	}
	b.WriteString(msg)
	for _, k := range keys {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString("=")
		fmt.Fprint(&b, rec[k])
	}
	return b.String()
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false,
		"follow today's log file as new lines are written")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 50,
		"number of trailing lines to show before following")
	logsCmd.Flags().StringVar(&logsRunID, "run-id", "",
		"filter to a single run (matches on prefix)")
	logsCmd.Flags().StringVar(&logsLevel, "level", "",
		"filter by log level: debug, info, warn, error")
	logsCmd.Flags().StringVar(&logsGrep, "grep", "",
		"substring filter applied to the raw JSON record")
	logsCmd.Flags().BoolVar(&logsAll, "all", false,
		"read every log file in the logs directory, not just today's")
	logsCmd.Flags().BoolVar(&logsPath, "path", false,
		"print the log directory path and exit")
	rootCmd.AddCommand(logsCmd)
}
