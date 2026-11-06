package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fs-relay/internal/local"
)

var errUsage = errors.New("usage")

// main runs the fs-relay command and exits with a status code on error.
func main() {
	if err := run(); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "fs-relay: %v\n", err)
		os.Exit(1)
	}
}

// run parses command-line flags, prepares the data directory, and serves HTTP.
func run() error {
	flags := flag.NewFlagSet("fs-relay", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8080", "address to listen on")
	data := flags.String("data", "data", "directory that holds one folder per namespace")
	given := flags.String("token", "", "shared secret; printed when omitted")
	file := flags.String("token-file", "", "read the shared secret from this file (mode 600)")
	forceMulti := flags.Bool("force-multi-serve", false, "serve a data directory that another fs-relay is already using")
	showVersion := flags.Bool("version", false, "print the version and exit")
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {}
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(usageText(flags))
			return nil
		}
		fmt.Fprint(os.Stderr, usageText(flags))
		return errUsage
	}
	if *showVersion {
		fmt.Println(versionLine("fs-relay"))
		return nil
	}

	var tokenSet, fileSet bool
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "token":
			tokenSet = true
		case "token-file":
			fileSet = true
		}
	})
	if tokenSet && fileSet {
		return fmt.Errorf("pass -token or -token-file, not both")
	}
	if tokenSet && *given == "" {
		return fmt.Errorf("pass a value for -token, or omit it and the server prints one")
	}
	if fileSet && *file == "" {
		return fmt.Errorf("pass a path for -token-file")
	}
	token, err := chooseToken(*given, *file)
	if err != nil {
		return err
	}
	tree := local.New(*data)
	defer tree.Close()
	if err := tree.Prepare(); err != nil {
		return err
	}
	if !*forceMulti {
		if err := tree.Lock(); err != nil {
			return err
		}
	}
	abs, err := filepath.Abs(*data)
	if err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *listen, err)
	}
	log.Printf("fs-relay: data %s", abs)
	log.Printf("fs-relay: listening on %s", listenURL(ln.Addr(), defaultRouteIP))
	h, raw := handler(tree, newHub(token))
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
	}
	srv.RegisterOnShutdown(raw.closeAll)
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigc)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	defer signal.Stop(sigc)
	select {
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-sigc:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shut := srv.Shutdown(ctx)
		<-errc
		return shut
	}
}
