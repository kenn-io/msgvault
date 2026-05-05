package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfirmDestructive_Permanent_LiteralDeleteAccepted(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("delete\n"), &out, ConfirmModePermanent)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Errorf("ok = false, want true after typing 'delete'")
	}
}

func TestConfirmDestructive_Permanent_NonDeleteRejected(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("y\n"), &out, ConfirmModePermanent)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false when input is 'y' under Permanent mode")
	}
	if !strings.Contains(out.String(), `Cancelled. Drop --permanent to use trash deletion without elevated permissions.`) {
		t.Errorf("output missing verbatim cancellation message: %q", out.String())
	}
}

func TestConfirmDestructive_Permanent_StdinClosed(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader(""), &out, ConfirmModePermanent)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Errorf("ok = true on closed stdin, want false")
	}
	if !strings.Contains(out.String(), `Cancelled. Drop --permanent to use trash deletion without elevated permissions.`) {
		t.Errorf("output missing verbatim cancellation message on EOF: %q", out.String())
	}
}

func TestConfirmDestructive_AllHidden_YesAccepted(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("y\n"), &out, ConfirmModeAllHidden)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Errorf("ok = false on 'y', want true under AllHidden mode")
	}
}

func TestConfirmDestructive_AllHidden_NoRejected(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("n\n"), &out, ConfirmModeAllHidden)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Errorf("ok = true on 'n', want false")
	}
}

func TestConfirmDestructive_AllHidden_StdinClosed(t *testing.T) {
	var out bytes.Buffer
	_, err := confirmDestructive(strings.NewReader(""), &out, ConfirmModeAllHidden)
	if err == nil {
		t.Fatalf("err = nil on closed stdin, want named error")
	}
	wantSubstr := "no confirmation input (stdin closed); --all-hidden cannot be skipped with --yes"
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("err = %q, want substring %q", err.Error(), wantSubstr)
	}
}

func TestConfirmDestructive_YesNo_YesAccepted(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("y\n"), &out, ConfirmModeYesNo)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Errorf("ok = false on 'y', want true under YesNo mode")
	}
}

func TestConfirmDestructive_YesNo_NoRejected(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader("n\n"), &out, ConfirmModeYesNo)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Errorf("ok = true on 'n', want false")
	}
}

func TestConfirmDestructive_YesNo_StdinClosed(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmDestructive(strings.NewReader(""), &out, ConfirmModeYesNo)
	if err != nil {
		t.Fatalf("err = %v on closed stdin, want nil (cancel-on-EOF)", err)
	}
	if ok {
		t.Errorf("ok = true on closed stdin, want false")
	}
}
