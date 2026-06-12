// gopls-fleet is an LSP stdio proxy that sits between an editor and gopls
// and dynamically narrows the gopls workspace to the directories the user is
// actually editing, via directoryFilters.
//
// In large single-module monorepos gopls type-checks every workspace package
// in the background after startup. The proxy starts gopls with everything
// excluded ("-**") and widens the scope as files are opened, so memory and
// CPU stay proportional to the dependency cones of the open services instead
// of the whole repository.
//
// Everything is local: no server, no shared cache.
//
//   - Opened files first get orphan-mode diagnostics from gopls, and the
//     workspace is re-scoped right after, so feedback stays fast.
//   - rename/references requests are held while the scope is expanded with
//     the reverse-import closure of the target package, so results are not
//     silently truncated to the current scope.
//   - The same binary acts as a GOPACKAGESDRIVER (when gopls invokes it) and
//     serves the package graph from an in-proxy cache, eliminating repeated
//     `go list ./...` runs on every re-scope.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

func main() {
	if os.Getenv("GOPLS_FLEET_DRIVER") == "1" {
		os.Exit(runDriver())
	}

	goplsPath := flag.String("gopls", "gopls", "path to the gopls binary")
	granularity := flag.Int("granularity", 3, "number of path segments that form one scope unit (e.g. 3 = go/services/auth)")
	debounce := flag.Duration("debounce", 500*time.Millisecond, "delay before applying a scope change, coalescing bursts of didOpen")
	driver := flag.Bool("driver", true, "serve the package graph to gopls via GOPACKAGESDRIVER (kills repeated go list runs)")
	logPath := flag.String("log", "", "debug log file (default: no logging)")
	flag.Parse()

	logger := log.New(io.Discard, "", log.LstdFlags|log.Lmicroseconds)
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gopls-fleet: open log: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		logger.SetOutput(f)
	}

	p := &proxy{
		granularity: *granularity,
		debounce:    *debounce,
		useDriver:   *driver,
		scope:       map[string]bool{},
		configIDs:   map[string]bool{},
		pendingDiag: map[string]bool{},
		idx:         newRevIndex(logger),
		log:         logger,
	}
	os.Exit(p.run(*goplsPath))
}
