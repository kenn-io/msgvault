package cmd

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatCLIProgressBar(t *testing.T) {
	tests := []struct {
		name  string
		pct   float64
		style cliProgressBarStyle
		want  string
	}{
		{
			name:  "default half",
			pct:   50,
			style: cliProgressBarStyle{Width: 30, Filled: "=", Empty: " "},
			want:  "[===============               ]",
		},
		{
			name:  "custom style",
			pct:   50,
			style: cliProgressBarStyle{Width: 10, Filled: "#", Empty: "-"},
			want:  "[#####-----]",
		},
		{
			name:  "clamps high",
			pct:   125,
			style: cliProgressBarStyle{Width: 4, Filled: "=", Empty: " "},
			want:  "[====]",
		},
		{
			name:  "clamps low",
			pct:   -10,
			style: cliProgressBarStyle{Width: 4, Filled: "=", Empty: " "},
			want:  "[    ]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatCLIProgressBar(tt.pct, tt.style))
		})
	}
}

func TestWriteCLIProgressPercent(t *testing.T) {
	var buf bytes.Buffer

	writeCLIProgressPercent(&buf, 2, 4)

	assert.Equal(t, "\r  [===============               ]  50%", buf.String())
}

func TestWriteCLIProgressPercentIgnoresUnknownTotal(t *testing.T) {
	var buf bytes.Buffer

	writeCLIProgressPercent(&buf, 2, 0)

	assert.Empty(t, buf.String())
}

func TestFormatCLIProgressDuration(t *testing.T) {
	tests := []struct {
		name  string
		d     time.Duration
		style cliProgressDurationStyle
		want  string
	}{
		{
			name:  "spaced seconds",
			d:     45 * time.Second,
			style: cliProgressDurationSpaced,
			want:  "45s",
		},
		{
			name:  "spaced minutes",
			d:     65 * time.Second,
			style: cliProgressDurationSpaced,
			want:  "1m 5s",
		},
		{
			name:  "spaced hours",
			d:     2*time.Hour + 3*time.Minute + 4*time.Second,
			style: cliProgressDurationSpaced,
			want:  "2h 3m",
		},
		{
			name:  "compact minutes",
			d:     65 * time.Second,
			style: cliProgressDurationCompactMinutes,
			want:  "1m05s",
		},
		{
			name:  "compact long minutes",
			d:     2*time.Hour + 3*time.Minute + 4*time.Second,
			style: cliProgressDurationCompactMinutes,
			want:  "123m04s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatCLIProgressDuration(tt.d, tt.style))
		})
	}
}
