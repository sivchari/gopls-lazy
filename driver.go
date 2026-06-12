package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"time"
)

// runDriver is the GOPACKAGESDRIVER entrypoint: gopls executes this binary
// with patterns as arguments and the DriverRequest on stdin. It forwards the
// query to the proxy's graph server over a unix socket and copies the answer
// back. Any failure degrades to NotHandled, which makes go/packages use its
// own go list driver — never worse than not having a driver at all.
func runDriver() int {
	req, err := io.ReadAll(os.Stdin)
	if err != nil {
		return writeNotHandled()
	}
	sock := os.Getenv("GOPLS_FLEET_SOCK")
	if sock == "" {
		return writeNotHandled()
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return writeNotHandled()
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	wd, _ := os.Getwd()
	q := driverQuery{Patterns: os.Args[1:], Dir: wd, Request: req}
	if err := json.NewEncoder(conn).Encode(q); err != nil {
		return writeNotHandled()
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	data, err := io.ReadAll(conn)
	if err != nil || len(data) == 0 {
		return writeNotHandled()
	}
	_, _ = os.Stdout.Write(data)
	return 0
}

func writeNotHandled() int {
	_, _ = os.Stdout.Write(notHandled)
	return 0
}
