//go:build linux

// msgvault-codex-bridge provides a loopback HTTP proxy endpoint inside the
// Codex network namespace. It forwards bytes to the daemon's Unix CONNECT
// policy service; only that service can open an upstream connection.
package main

import (
	"io"
	"net"
	"os"
	"os/exec"
)

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:3128")
	if err != nil {
		os.Exit(1)
	}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go relay(client)
		}
	}()
	command := exec.Command("/codex", os.Args[1:]...) //nolint:gosec // The launcher fixes /codex and validates the app-server arguments.
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		os.Exit(1)
	}
}

func relay(client net.Conn) {
	defer func() { _ = client.Close() }()
	upstream, err := net.Dial("unix", "/work/.proxy.sock")
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, client)
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	_ = upstream.Close()
	<-done
}
