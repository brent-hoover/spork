// Command sutra serves the issue tracker's HTTP API.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"sutra/internal/api"
	"sutra/internal/cli"
	"sutra/internal/web"
)

func main() {
	// CLI subcommands dispatch to the CLI client; anything else is the
	// daemon. `sutra init` must never start a server.
	if len(os.Args) > 1 && isCLICommand(os.Args[1]) {
		workdir, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		os.Exit(cli.Run(cli.Env{
			Args:    os.Args[1:],
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
			Workdir: workdir,
			Getenv:  os.Getenv,
		}))
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// isCLICommand reports whether the first argument selects the CLI
// surface rather than the daemon.
func isCLICommand(arg string) bool {
	switch arg {
	case "init", "issue", "api":
		return true
	}
	return false
}

func run() error {
	addr := flag.String("addr", envOr("SUTRA_ADDR", "127.0.0.1:7357"), "listen address")
	dbPath := flag.String("db", envOr("SUTRA_DB", "sutra.db"), "SQLite database path")
	uiAddr := flag.String("ui-addr", envOr("SUTRA_UI_ADDR", ""), "web UI listen address (off when empty)")
	uiActor := flag.String("ui-actor", envOr("SUTRA_UI_ACTOR", ""), "identity id web UI mutations act as")
	uiHosts := flag.String("ui-hosts", envOr("SUTRA_UI_HOSTS", ""), "comma-separated canonical UI hosts; mutations from any other Host are refused (DNS-rebinding guard)")
	flag.Parse()

	db, err := sql.Open("sqlite", "file:"+escapeSQLitePath(*dbPath)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return fmt.Errorf("open %s: %w", *dbPath, err)
	}
	defer func() { _ = db.Close() }()

	handler, err := api.New(db)
	if err != nil {
		return fmt.Errorf("wire api: %w", err)
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// The full-request read bound: a slow or stalled upload can
		// never occupy a handler indefinitely. Five minutes clears any
		// legitimate localhost payload within the physical body bounds.
		ReadTimeout: 5 * time.Minute,
		// Streaming reads (export, listings) hold a read snapshot while
		// writing; the write bound keeps a stalled client from pinning
		// the WAL indefinitely. Ten minutes clears any localhost export.
		WriteTimeout: 10 * time.Minute,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The web UI is a pure HTTP client of the API (MOD-web imports no
	// sibling modules); it serves on its own listener when enabled.
	var uiServer *http.Server
	if *uiAddr != "" {
		uiServer = &http.Server{
			Addr: *uiAddr,
			// The UI's own bind address is ALWAYS canonical; extra
			// hosts (a reverse proxy's name) add to it. The allowlist
			// is never empty, so the rebinding guard always applies.
			Handler:           web.New("http://"+*addr, *uiActor, append([]string{*uiAddr}, splitHosts(*uiHosts)...)...).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Printf("sutra web UI on %s", *uiAddr)
			if err := uiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("web UI: %v", err)
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("sutra serving on %s (db %s)", *addr, *dbPath)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	case <-shutdownCtx.Done():
		log.Printf("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if uiServer != nil {
			_ = uiServer.Shutdown(ctx)
		}
		if err := server.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
}

// splitHosts parses the comma-separated canonical UI host list.
func splitHosts(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if h := strings.TrimSpace(p); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// escapeSQLitePath percent-encodes the characters SQLite's URI parser
// would otherwise misread as syntax — %, ?, # and & — so a database
// path containing them opens the intended file instead of a mangled
// one. Slashes stay literal: they are path structure.
func escapeSQLitePath(p string) string {
	r := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23", "&", "%26")
	return r.Replace(p)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
