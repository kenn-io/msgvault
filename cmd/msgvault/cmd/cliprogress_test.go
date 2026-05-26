package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCLIProgress_OnLatestDateBeforeOnStart(t *testing.T) {
	p := &CLIProgress{}
	p.OnLatestDate(time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))

	require.False(t, p.startTime.IsZero(), "startTime should be initialized when OnLatestDate is called before OnStart")
	require.LessOrEqual(t, time.Since(p.startTime), time.Second, "startTime should be recent")
}

func TestCLIProgress_OnProgressBeforeOnStart(t *testing.T) {
	p := &CLIProgress{}
	p.OnProgress(10, 5, 3)

	require.False(t, p.startTime.IsZero(), "startTime should be initialized when OnProgress is called before OnStart")
	require.LessOrEqual(t, time.Since(p.startTime), time.Second, "startTime should be recent")
}

func TestCLIProgress_OnStartResetsForReuse(t *testing.T) {
	p := &CLIProgress{}
	p.OnStart(100)
	first := p.startTime

	time.Sleep(5 * time.Millisecond)
	p.OnStart(200)

	require.True(t, p.startTime.After(first), "OnStart should reset startTime on subsequent calls")
}
