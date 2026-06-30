//go:build windows

package cmd

import (
	"flag"
	"os"
	"testing"
	"time"
)

const windowsCommandPackageTimeout = 20 * time.Minute

func TestMain(m *testing.M) {
	flag.Parse()
	raiseWindowsCommandPackageTimeout()
	os.Exit(m.Run())
}

func raiseWindowsCommandPackageTimeout() {
	timeoutFlag := flag.Lookup("test.timeout")
	if timeoutFlag == nil {
		return
	}
	current, err := time.ParseDuration(timeoutFlag.Value.String())
	if err != nil || current <= 0 || current >= windowsCommandPackageTimeout {
		return
	}
	_ = timeoutFlag.Value.Set(windowsCommandPackageTimeout.String())
}
