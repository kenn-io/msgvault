package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	Long: `Show msgvault's structured log output from the selected daemon's
on-disk logs: the JSON logs under <data dir>/logs and the background
daemon's <data dir>/serve.log. With [remote].url configured, this shows the
remote daemon's logs; otherwise it starts or contacts the local daemon.

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
	Args: cobra.NoArgs,
	RunE: runLogsCmd,
}

// validLogLevels lists the accepted --level values, matching slog's levels.
var validLogLevels = []string{"debug", "info", "warn", "error"}

func runLogsCmd(cmd *cobra.Command, args []string) error {
	// Validate --level up front (on the client, before proxying to the
	// daemon) so a typo fails fast with the allowed set instead of
	// silently matching nothing.
	if logsLevel != "" && !slices.Contains(validLogLevels, strings.ToLower(logsLevel)) {
		return usageErr(cmd, fmt.Errorf(
			"invalid --level: %q (want one of: %s)",
			logsLevel, strings.Join(validLogLevels, ", "),
		))
	}

	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}

	dir := cfg.LogsDir()
	serveLogPath := filepath.Join(cfg.Data.DataDir, "serve.log")

	if logsPath {
		fmt.Println(dir)
		return nil
	}

	files, err := findLogFiles(dir, serveLogPath, logsAll)
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

// findLogFiles returns the sorted list of log files to read. The daemon's
// serveLogPath (the background daemon's serve.log, written outside the logs
// dir) is always included when it exists, so `logs` can surface the running
// daemon's output. When all is false, it returns today's structured log file
// (if present) plus serve.log; when all is true it returns every regular file
// in the logs directory (not just the msgvault-YYYY-MM-DD.log pattern) plus
// serve.log.
func findLogFiles(dir, serveLogPath string, all bool) ([]string, error) {
	appendIfExists := func(files []string, path string) []string {
		if path == "" {
			return files
		}
		if _, err := os.Stat(path); err == nil {
			return append(files, path)
		}
		return files
	}

	dirFiles, err := logDirFiles(dir, all)
	if err != nil {
		return nil, err
	}

	files := appendIfExists(dirFiles, serveLogPath)
	sort.Slice(files, func(i, j int) bool {
		return logFileSortKey(files[i]) < logFileSortKey(files[j])
	})
	return files, nil
}

// logDirFiles returns the log files inside dir. When all is false it returns
// today's structured file if it exists (falling through to the full scan
// otherwise); when all is true it returns every regular file in the dir.
func logDirFiles(dir string, all bool) ([]string, error) {
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
		// Fall through to the full scan; maybe we only have
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
		name := e.Name()
		// The unrestricted scan (every regular file) is only for --all. The
		// non-all fallback (today's structured log missing) must stay limited
		// to structured msgvault-*.log files so `logs` never surfaces
		// unrelated files that happen to sit in the logs dir.
		if !all && !strings.HasPrefix(name, "msgvault-") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
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

// renderLogLine decodes a single raw log line and, if it passes filter,
// returns its human-readable form. It accepts JSON records (the structured
// logs directory) and logfmt records (the daemon's serve.log); lines that are
// neither (e.g. serve.log restart banners) are passed through verbatim as long
// as no level filter is active.
func renderLogLine(raw []byte, filter logFilter) (string, bool) {
	if rec, ok := parseLogRecord(raw); ok {
		if !filter.matches(raw, rec) {
			return "", false
		}
		return formatLogRecord(rec), true
	}
	// An unstructured line carries no level, so a level filter excludes it.
	if filter.Level != "" {
		return "", false
	}
	if filter.Grep != "" && !strings.Contains(string(raw), filter.Grep) {
		return "", false
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", false
	}
	return string(raw), true
}

// parseLogRecord decodes a log line as either a JSON object (structured logs)
// or a logfmt line (the slog text handler used for serve.log).
func parseLogRecord(raw []byte) (map[string]any, bool) {
	var rec map[string]any
	if json.Unmarshal(raw, &rec) == nil && rec != nil {
		return rec, true
	}
	return parseLogfmtRecord(raw)
}

// parseLogfmtRecord parses a slog text-handler (logfmt) line into a record.
// It returns ok=false for lines that are not key=value structured logs.
func parseLogfmtRecord(raw []byte) (map[string]any, bool) {
	s := string(raw)
	rec := make(map[string]any)
	for i := 0; i < len(s); {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		keyStart := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			return nil, false
		}
		key := s[keyStart:i]
		i++ // consume '='
		var val string
		val, i = readLogfmtValue(s, i)
		rec[key] = val
	}
	for _, k := range []string{"msg", "level", "time"} {
		if _, ok := rec[k]; ok {
			return rec, true
		}
	}
	return nil, false
}

// readLogfmtValue reads a logfmt value starting at index i, honoring Go-style
// double-quoted values, and returns the value with the index just past it.
func readLogfmtValue(s string, i int) (string, int) {
	if i >= len(s) || s[i] != '"' {
		start := i
		for i < len(s) && s[i] != ' ' {
			i++
		}
		return s[start:i], i
	}
	var b strings.Builder
	i++ // opening quote
	for i < len(s) {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1])
			i += 2
			continue
		}
		if c == '"' {
			i++ // closing quote
			break
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), i
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
			if line, ok := renderLogLine(scanner.Bytes(), filter); ok {
				push(line)
			}
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
				trimmed := line[:len(line)-1]
				if rendered, ok := renderLogLine(trimmed, filter); ok {
					_, _ = fmt.Fprintln(out, rendered)
				}
				continue
			}
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("read log line: %w", err)
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
