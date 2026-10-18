package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/arisros/tangkar/internal/server"
	"github.com/arisros/tangkar/internal/store"
	"github.com/arisros/tangkar/internal/tui"
	"github.com/arisros/tangkar/internal/wa"
	"github.com/arisros/tangkar/web"
)

var version = "dev"

const usage = `tangkar, soto tangkar orders

  tangkar               open the TUI (same as "tangkar tui")
  tangkar tui           open the TUI
  tangkar logout        forget this terminal's login
  tangkar serve         run the server
  tangkar version

TUI env: TANGKAR_URL (default https://tangkar.arisjirat.com)
`

func main() {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	cmd := "tui"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "tui":
		err = tui.Run(env("TANGKAR_URL", "https://tangkar.arisjirat.com"))
	case "logout":
		err = tui.Logout(env("TANGKAR_URL", "https://tangkar.arisjirat.com"))
	case "serve":
		err = serve()
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		fmt.Print(usage)
		if cmd != "help" && cmd != "-h" && cmd != "--help" {
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tangkar:", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func serve() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	base := strings.TrimRight(env("TANGKAR_BASE_URL", "http://localhost:8080"), "/")
	admins := map[string]bool{}
	for _, a := range strings.Split(os.Getenv("TANGKAR_ADMINS"), ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			admins[a] = true
		}
	}
	cfg := server.Config{
		BaseURL:            base,
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		SessionSecret:      os.Getenv("TANGKAR_SESSION_SECRET"),
		AllowedDomain:      env("TANGKAR_DOMAIN", "bfi.co.id"),
		Admins:             admins,
		SecureCookie:       strings.HasPrefix(base, "https://"),
		DevLogin:           os.Getenv("TANGKAR_DEV_LOGIN") == "1",
		WATo:               os.Getenv("TANGKAR_WA_TO"),
		WATestTo:           os.Getenv("TANGKAR_WA_TEST_TO"),
		WADryRun:           os.Getenv("TANGKAR_WA_DRY_RUN") == "1",
	}
	if cfg.DevLogin && cfg.SecureCookie {
		return errors.New("TANGKAR_DEV_LOGIN is for local http runs only")
	}
	st, err := store.Open(env("TANGKAR_DB", "tangkar.db"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer st.Close()
	srv, err := server.New(cfg, st, wa.New(os.Getenv("TANGKAR_WA_URL"), os.Getenv("TANGKAR_WA_TOKEN")), web.FS, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go srv.Run(ctx)

	hs := &http.Server{
		Addr:              env("TANGKAR_ADDR", ":8080"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("tangkar serving", "addr", hs.Addr, "base", base, "version", version, "wa_to", cfg.WATo != "", "dry_run", cfg.WADryRun)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
