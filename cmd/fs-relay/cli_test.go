package main

import (
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"fs-relay/internal/local"
)

func TestVersionLine(t *testing.T) {
	version = "v1.2.3"
	defer func() { version = "" }()
	if got := versionLine("fs-relay"); got != "fs-relay v1.2.3" {
		t.Fatalf("version %q", got)
	}
	version = ""
	if got := versionLine("fs-relay"); !strings.HasPrefix(got, "fs-relay ") || len(got) < len("fs-relay x") {
		t.Fatalf("fallback version %q", got)
	}
}

func TestUsageTextVersion(t *testing.T) {
	version = "v1.2.3"
	defer func() { version = "" }()
	fs := flag.NewFlagSet("fs-relay", flag.ContinueOnError)
	if got := usageText(fs); !strings.Contains(got, "Version:\n  fs-relay v1.2.3") {
		t.Fatalf("usage missing version:\n%s", got)
	}
}

// TestForceMultiServeFlag verifies that run refuses an already-locked data
// directory by default and starts cleanly when -force-multi-serve is set.
func TestForceMultiServeFlag(t *testing.T) {
	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })

	root := filepath.Join(t.TempDir(), "data")
	first := local.New(root)
	t.Cleanup(func() { _ = first.Close() })
	if err := first.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := first.Lock(); err != nil {
		t.Fatal(err)
	}

	os.Args = []string{"fs-relay", "-data", root, "-token", "local-demo", "-listen", "127.0.0.1:0"}
	if err := run(); err == nil || !strings.Contains(err.Error(), "is already served by pid ") {
		t.Fatalf("run without -force-multi-serve = %v, want already served by pid error", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	os.Args = []string{"fs-relay", "-data", root, "-token", "local-demo", "-listen", addr, "-force-multi-serve"}
	errc := make(chan error, 1)
	go func() { errc <- run() }()
	for {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		select {
		case runErr := <-errc:
			t.Fatalf("run exited early: %v", runErr)
		default:
			runtime.Gosched()
		}
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("run with -force-multi-serve: %v", err)
	}
}
