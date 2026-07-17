package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const cliProgressBarWidth = 30

type cliProgressBarStyle struct {
	Width  int
	Filled string
	Empty  string
}

type cliProgressDurationStyle int

const (
	cliProgressDurationSpaced cliProgressDurationStyle = iota
	cliProgressDurationCompactMinutes
)

var (
	cliPercentProgressStyle = cliProgressBarStyle{
		Width:  cliProgressBarWidth,
		Filled: "=",
		Empty:  " ",
	}
	cliDeletionProgressStyle = cliProgressBarStyle{
		Width:  cliProgressBarWidth,
		Filled: "#",
		Empty:  "-",
	}
)

func formatCLIProgressDuration(d time.Duration, style cliProgressDurationStyle) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second

	if style == cliProgressDurationCompactMinutes {
		if d < time.Minute {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}

	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func formatCLIProgressBar(pct float64, style cliProgressBarStyle) string {
	if style.Width <= 0 {
		style.Width = cliProgressBarWidth
	}
	if style.Filled == "" {
		style.Filled = "="
	}
	if style.Empty == "" {
		style.Empty = " "
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	filled := min(int(pct/100*float64(style.Width)), style.Width)
	var b strings.Builder
	b.WriteString("[")
	for i := range style.Width {
		if i < filled {
			b.WriteString(style.Filled)
		} else {
			b.WriteString(style.Empty)
		}
	}
	b.WriteString("]")
	return b.String()
}

func writeCLIProgressPercent(w io.Writer, done, total int64) {
	if total <= 0 {
		return
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	pct := int(done * 100 / total)
	_, _ = fmt.Fprintf(w,
		"\r  %s %3d%%",
		formatCLIProgressBar(float64(pct), cliPercentProgressStyle),
		pct,
	)
}

func writeCLIIndexedProgressComplete(w io.Writer, indexed int64) {
	_, _ = fmt.Fprintf(w,
		"\r  %s 100%%  %d messages indexed.\n",
		formatCLIProgressBar(100, cliPercentProgressStyle), indexed)
}
