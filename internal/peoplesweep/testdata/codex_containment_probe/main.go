package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// This executable is built statically by the opt-in Codex containment test.
// Codex command/exec runs it inside the production Bubblewrap boundary.
func main() {
	if len(os.Args) != 3 && !(len(os.Args) == 4 && os.Args[1] == "copy") {
		fmt.Println("INVALID_ARGUMENTS")
		os.Exit(2)
	}
	var (
		value string
		err   error
	)
	switch os.Args[1] {
	case "read":
		var contents []byte
		contents, err = os.ReadFile(os.Args[2])
		value = string(contents)
	case "write":
		err = os.WriteFile(os.Args[2], []byte("SYNTHETIC_PROBE_WRITTEN"), 0o600)
	case "copy":
		var contents []byte
		contents, err = os.ReadFile(os.Args[2])
		if err == nil {
			err = os.WriteFile(os.Args[3], contents, 0o600)
		}
	case "egress":
		var connection net.Conn
		connection, err = net.DialTimeout("tcp", os.Args[2], 500*time.Millisecond)
		if connection != nil {
			_ = connection.Close()
		}
	case "proxy":
		var connection net.Conn
		connection, err = net.DialTimeout("tcp", "127.0.0.1:3128", 500*time.Millisecond)
		if err == nil {
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
			_, err = fmt.Fprintf(connection, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", os.Args[2], os.Args[2])
			if err == nil {
				value, err = bufio.NewReader(connection).ReadString('\n')
			}
			if err == nil {
				value = value[:len(value)-2]
			}
		}
	case "tls":
		connection, dialErr := net.DialTimeout("tcp", "127.0.0.1:3128", 2*time.Second)
		if dialErr != nil {
			fmt.Println("TLS_PROXY_FAILED")
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(8 * time.Second))
		_, err = fmt.Fprintf(connection, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", os.Args[2], os.Args[2])
		if err != nil {
			fmt.Println("TLS_PROXY_FAILED")
			return
		}
		reader := bufio.NewReader(connection)
		status, readErr := reader.ReadString('\n')
		if readErr != nil || status != "HTTP/1.1 200 Connection Established\r\n" {
			fmt.Println("TLS_PROXY_FAILED")
			return
		}
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				fmt.Println("TLS_PROXY_FAILED")
				return
			}
			if line == "\r\n" {
				break
			}
		}
		host, _, splitErr := net.SplitHostPort(os.Args[2])
		if splitErr != nil {
			fmt.Println("TLS_PROXY_FAILED")
			return
		}
		secure := tls.Client(connection, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if handshakeErr := secure.HandshakeContext(context.Background()); handshakeErr != nil {
			var unknownAuthority x509.UnknownAuthorityError
			if errors.As(handshakeErr, &unknownAuthority) {
				fmt.Println("TLS_UNTRUSTED")
			} else {
				fmt.Println("TLS_HANDSHAKE_FAILED")
			}
			return
		}
		fmt.Println("TLS_VERIFIED")
		return
	default:
		fmt.Println("INVALID_OPERATION")
		os.Exit(2)
	}
	if err == nil {
		if os.Args[1] == "proxy" {
			if value == "HTTP/1.1 403 Forbidden" {
				fmt.Printf("DENIED %s\n", value)
			} else {
				fmt.Printf("ALLOWED %s\n", value)
			}
			return
		}
		fmt.Printf("ALLOWED %s\n", value)
		return
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		fmt.Printf("DENIED errno=%d\n", errno)
		return
	}
	fmt.Println("DENIED non-syscall-error")
}
