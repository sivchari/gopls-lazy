// gopls-lazy is an LSP stdio proxy that sits between an editor and gopls
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
//   - rename/references/implementation requests are resolved to their
//     defining package and held while the scope is expanded with its
//     reverse-import closure (or the whole workspace for method symbols,
//     which can be referenced through interfaces from anywhere), so results
//     are not silently truncated.
//   - Scope units with no open files are evicted after a TTL.
//   - The same binary acts as a GOPACKAGESDRIVER (when gopls invokes it) and
//     serves the package graph from an in-proxy cache, eliminating repeated
//     `go list ./...` runs on every re-scope.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"
)

type options struct {
	gopls       string
	granularity int
	debounce    time.Duration
	evictTTL    time.Duration // 0 disables eviction
	driver      bool
	logPath     string
	goplsArgs   []string // unrecognized flags, forwarded to gopls
}

// parseArgs understands gopls-lazy's own flags and forwards everything else
// to gopls (editors like VS Code pass extra flags to the configured "gopls"
// binary). Defaults can also come from GOPLS_LAZY_* environment variables,
// for editor configs that cannot pass arguments.
func parseArgs(args []string, getenv func(string) string) (options, error) { //nolint:gocognit // flag parser with many env vars and flags is inherently complex
	o := options{
		gopls:       "gopls",
		granularity: 3,
		debounce:    500 * time.Millisecond,
		evictTTL:    10 * time.Minute,
		driver:      true,
	}
	if v := getenv("GOPLS_LAZY_GOPLS"); v != "" {
		o.gopls = v
	}
	if v := getenv("GOPLS_LAZY_GRANULARITY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return o, fmt.Errorf("GOPLS_LAZY_GRANULARITY: %w", err)
		}
		o.granularity = n
	}
	if v := getenv("GOPLS_LAZY_DEBOUNCE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return o, fmt.Errorf("GOPLS_LAZY_DEBOUNCE: %w", err)
		}
		o.debounce = d
	}
	if v := getenv("GOPLS_LAZY_EVICT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return o, fmt.Errorf("GOPLS_LAZY_EVICT: %w", err)
		}
		o.evictTTL = d
	}
	if v := getenv("GOPLS_LAZY_DRIVER"); v != "" && v != "1" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return o, fmt.Errorf("GOPLS_LAZY_DRIVER: %w", err)
		}
		o.driver = b
	}
	if v := getenv("GOPLS_LAZY_LOG"); v != "" {
		o.logPath = v
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := arg, "", false
		if k, v, ok := cutFlagValue(arg); ok {
			name, value, hasValue = k, v, true
		}
		next := func() (string, error) {
			if hasValue {
				return value, nil
			}
			i++
			if i >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", name)
			}
			return args[i], nil
		}
		var err error
		switch name {
		case "-gopls", "--gopls":
			o.gopls, err = next()
		case "-granularity", "--granularity":
			var v string
			if v, err = next(); err == nil {
				o.granularity, err = strconv.Atoi(v)
			}
		case "-debounce", "--debounce":
			var v string
			if v, err = next(); err == nil {
				o.debounce, err = time.ParseDuration(v)
			}
		case "-evict", "--evict":
			var v string
			if v, err = next(); err == nil {
				o.evictTTL, err = time.ParseDuration(v)
			}
		case "-driver", "--driver":
			if hasValue {
				o.driver, err = strconv.ParseBool(value)
			} else {
				o.driver = true
			}
		case "-log", "--log":
			o.logPath, err = next()
		default:
			o.goplsArgs = append(o.goplsArgs, arg)
		}
		if err != nil {
			return o, fmt.Errorf("flag %s: %w", name, err)
		}
	}
	return o, nil
}

func cutFlagValue(arg string) (name, value string, ok bool) {
	if len(arg) == 0 || arg[0] != '-' {
		return arg, "", false
	}
	for i := 1; i < len(arg); i++ {
		if arg[i] == '=' {
			return arg[:i], arg[i+1:], true
		}
	}
	return arg, "", false
}

func main() {
	if os.Getenv("GOPLS_LAZY_DRIVER") == "1" && os.Getenv("GOPLS_LAZY_SOCK") != "" {
		os.Exit(runDriver())
	}

	opts, err := parseArgs(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopls-lazy: %v\n", err)
		os.Exit(2)
	}

	logger := log.New(io.Discard, "", log.LstdFlags|log.Lmicroseconds)
	if opts.logPath != "" {
		f, err := os.OpenFile(opts.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gopls-lazy: open log: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = f.Close() }()
		logger.SetOutput(f)
	}

	p := &proxy{
		opts:        opts,
		scope:       map[string]*scopeEntry{},
		configIDs:   map[string]bool{},
		pendingDiag: map[string]bool{},
		pendingOwn:  map[string]chan *message{},
		idx:         newRevIndex(logger),
		log:         logger,
	}
	os.Exit(p.run())
}
