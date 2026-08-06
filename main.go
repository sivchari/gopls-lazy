// Package goplslazy implements an LSP stdio proxy that sits between an editor
// and gopls and dynamically narrows the gopls workspace to the directories the
// user is actually editing, via directoryFilters.
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
//   - rename/references requests are resolved to their defining package and
//     held while the scope is expanded with its reverse-import closure, so
//     results are not silently truncated.
//   - implementation requests and method-wide references run in an isolated
//     worker gopls process, so the long-lived interactive gopls is never
//     widened to the whole workspace.
//   - Scope units with no open files are evicted after a TTL.
//   - The same binary acts as a GOPACKAGESDRIVER (when gopls invokes it) and
//     serves the package graph from an in-proxy cache, eliminating repeated
//     `go list ./...` runs on every re-scope.
package goplslazy

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"
)

type options struct {
	gopls         string
	granularity   int
	debounce      time.Duration
	evictTTL      time.Duration // 0 disables eviction
	driver        bool
	logPath       string
	workerTTL     time.Duration // idle time before the whole-workspace worker is evicted; 0 disables
	workerTimeout time.Duration // budget for a single worker query
	workerWarm    bool          // start the worker at initialize instead of on first query
	goplsArgs     []string      // unrecognized flags, forwarded to gopls
}

// applyEnv overrides option defaults from GOPLS_LAZY_* environment variables,
// for editor configs that cannot pass command-line flags.
func applyEnv(o *options, getenv func(string) string) error {
	if v := getenv("GOPLS_LAZY_GOPLS"); v != "" {
		o.gopls = v
	}
	if v := getenv("GOPLS_LAZY_GRANULARITY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("GOPLS_LAZY_GRANULARITY: %w", err)
		}
		o.granularity = n
	}
	durations := []struct {
		key string
		dst *time.Duration
	}{
		{"GOPLS_LAZY_DEBOUNCE", &o.debounce},
		{"GOPLS_LAZY_EVICT", &o.evictTTL},
		{"GOPLS_LAZY_WORKER_TTL", &o.workerTTL},
		{"GOPLS_LAZY_WORKER_TIMEOUT", &o.workerTimeout},
	}
	for _, e := range durations {
		if v := getenv(e.key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", e.key, err)
			}
			*e.dst = d
		}
	}
	if v := getenv("GOPLS_LAZY_DRIVER"); v != "" && v != "1" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOPLS_LAZY_DRIVER: %w", err)
		}
		o.driver = b
	}
	if v := getenv("GOPLS_LAZY_LOG"); v != "" {
		o.logPath = v
	}
	if v := getenv("GOPLS_LAZY_WORKER_WARM"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GOPLS_LAZY_WORKER_WARM: %w", err)
		}
		o.workerWarm = b
	}
	return nil
}

// parseArgs understands gopls-lazy's own flags and forwards everything else
// to gopls (editors like VS Code pass extra flags to the configured "gopls"
// binary). Defaults can also come from GOPLS_LAZY_* environment variables,
// for editor configs that cannot pass arguments.
func parseArgs(args []string, getenv func(string) string) (options, error) { //nolint:gocognit,cyclop // the flag switch dispatches many flags and is clearer as one linear block than fragmented across helpers
	o := options{
		gopls:         "gopls",
		granularity:   3,
		debounce:      500 * time.Millisecond,
		evictTTL:      10 * time.Minute,
		driver:        true,
		workerTTL:     15 * time.Minute,
		workerTimeout: 10 * time.Minute,
	}
	if err := applyEnv(&o, getenv); err != nil {
		return o, err
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
		case "-worker-ttl", "--worker-ttl":
			var v string
			if v, err = next(); err == nil {
				o.workerTTL, err = time.ParseDuration(v)
			}
		case "-worker-timeout", "--worker-timeout":
			var v string
			if v, err = next(); err == nil {
				o.workerTimeout, err = time.ParseDuration(v)
			}
		case "-worker-warm", "--worker-warm":
			if hasValue {
				o.workerWarm, err = strconv.ParseBool(value)
			} else {
				o.workerWarm = true
			}
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
	if arg == "" || arg[0] != '-' {
		return arg, "", false
	}
	for i := 1; i < len(arg); i++ {
		if arg[i] == '=' {
			return arg[:i], arg[i+1:], true
		}
	}
	return arg, "", false
}

// Run is the entry point for the gopls-lazy proxy. It returns a process exit
// code; the caller passes it to os.Exit so that deferred cleanup (e.g. closing
// the log file) runs before the process terminates.
func Run() int {
	if os.Getenv("GOPLS_LAZY_DRIVER") == "1" && os.Getenv("GOPLS_LAZY_SOCK") != "" {
		return runDriver()
	}

	opts, err := parseArgs(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gopls-lazy: %v\n", err)
		return 2
	}

	logger := log.New(io.Discard, "", log.LstdFlags|log.Lmicroseconds)
	if opts.logPath != "" {
		f, err := os.OpenFile(opts.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gopls-lazy: open log: %v\n", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		logger.SetOutput(f)
	}

	p := &proxy{
		opts:        opts,
		scope:       map[string]*scopeEntry{},
		configIDs:   map[string]bool{},
		openDocs:    map[string]openDoc{},
		pendingDiag: map[string]bool{},
		pendingOwn:  map[string]chan *message{},
		idx:         newRevIndex(logger),
		modRoots:    &moduleRootCache{},
		log:         logger,
	}
	p.worker = &workerHandle{p: p}
	return p.run()
}
