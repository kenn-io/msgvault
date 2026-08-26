package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"time"
)

var (
	version       = "codex-cli 0.149.0"
	versionBase64 string
	mode          string
	markerBase64  string
	secret        string
)

func main() {
	marker, _ := base64.StdEncoding.DecodeString(markerBase64)
	if len(os.Args) > 1 && os.Args[1] == "fixture-descendant" {
		time.Sleep(time.Second)
		_ = os.WriteFile(string(marker), []byte("late"), 0o600)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		runVersion(marker)
		return
	}
	if mode == "app-server-descendant" {
		command := exec.Command(os.Args[0], "fixture-descendant")
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			os.Exit(1)
		}
		_ = os.WriteFile(string(marker)+".ready", []byte("ready"), 0o600)
		time.Sleep(10 * time.Second)
		return
	}
	if len(marker) > 0 {
		_ = os.WriteFile(string(marker), []byte("app-server"), 0o600)
	}
}

func runVersion(marker []byte) {
	if encodedVersion, err := base64.StdEncoding.DecodeString(versionBase64); err == nil && len(encodedVersion) > 0 {
		version = string(encodedVersion)
	}
	switch mode {
	case "marker":
		_ = os.WriteFile(string(marker), []byte("version"), 0o600)
	case "hang":
		fmt.Println(secret)
		time.Sleep(time.Second)
		_ = os.WriteFile(string(marker), []byte("late"), 0o600)
	case "descendant":
		command := exec.Command(os.Args[0], "fixture-descendant")
		command.Stdout = os.Stdout
		_ = command.Start()
	}
	fmt.Println(version)
}
