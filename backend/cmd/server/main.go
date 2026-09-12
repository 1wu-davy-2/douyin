// Command server is the Douyin Archive v2 main backend: HTTP API + SSE +
// embedded SPA, SQLite persistence, on-demand Python signing sidecar.
//
// Environment: see internal/config. Quick start (mock mode):
//
//	DY_MOCK=1 go run ./backend/cmd/server
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
	"sync"
	"syscall"
	"time"

	"douyin/backend/internal/api"
	"douyin/backend/internal/auth"
	"douyin/backend/internal/config"
	"douyin/backend/internal/db"
	"douyin/backend/internal/downloader"
	"douyin/backend/internal/events"
	"douyin/backend/internal/provider"
	"douyin/backend/internal/scanner"
	"douyin/backend/internal/scheduler"
	"douyin/backend/internal/settings"
	"douyin/backend/internal/sidecar"
)

func main() {
	// -mode is parsed first: import-legacy is not part of stage 1.
	mode := flag.String("mode", "", "special run mode (import-legacy: not implemented yet)")
	flag.Parse()
	if *mode != "" {
		fmt.Println("not implemented yet")
		os.Exit(0)
	}

	log.SetFlags(log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, config.Load()); err != nil {
		log.Fatalf("server: %v", err)
	}
	log.Printf("bye")
}

// run wires every component together and serves until ctx is canceled. It
// shuts down in order: HTTP (bounded — SSE streams are force-closed after the
// grace period), sidecar process, database.
func run(ctx context.Context, cfg config.Settings) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir %s: %w", cfg.DataDir, err)
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close() // second close is a no-op; first runs at shutdown
	if err := db.Migrate(database); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	bus := events.New()

	mgr := sidecar.NewManager(cfg.SidecarPort, cfg.SidecarPython, cfg.Mock, cfg.SidecarIdleTimeout)
	defer mgr.Stop()
	mgr.SetOnStatus(func(state string) {
		// provider.status is a state-class event: must reach every subscriber.
		bus.Publish(events.Event{
			Type: events.TypeProviderStatus,
			Data: events.ProviderStatus{Sidecar: state},
		})
	})

	store := settings.NewStore(database, cfg)
	store.SetOnSidecarIdleTimeout(mgr.SetIdleTimeout)

	resolver := provider.NewResolver(cfg, mgr, store)
	authService := auth.New(database)

	// Scanner (stage 4): owns scan runs, single-flight, SSE progress events.
	scanSvc := scanner.New(ctx, resolver, database, bus, store)
	if err := scanSvc.RecycleStaleRuns(); err != nil {
		log.Printf("recycle stale scan runs: %v", err)
	}

	// Downloader (stage 5): persistent job queue + worker pool. Recover rows
	// left downloading by a previous process, then start the pool and wire it
	// as the scanner's auto-download enqueuer.
	dl := downloader.New(ctx, downloader.Deps{
		DB:      database,
		Bus:     bus,
		Store:   store,
		Source:  resolver,
		DataDir: cfg.DataDir,
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.Port),
	})
	if _, err := dl.RecoverStale(); err != nil {
		log.Printf("recover stale download jobs: %v", err)
	}
	if err := dl.Start(); err != nil {
		return fmt.Errorf("start downloader: %w", err)
	}
	store.SetOnDownloadConcurrency(dl.SetConcurrency)
	scanSvc.SetEnqueuer(dl)

	// Scheduler (stage 4): single goroutine servicing due subscriptions.
	sched := scheduler.New(database, scanSvc, store)
	var schedWG sync.WaitGroup
	schedWG.Add(1)
	go func() {
		defer schedWG.Done()
		sched.Run(ctx)
	}()

	srv := &http.Server{
		// Local single-user tool; loopback only per README.
		Addr: fmt.Sprintf("127.0.0.1:%d", cfg.Port),
		Handler: api.New(api.Deps{
			Cfg:        cfg,
			Auth:       authService,
			Bus:        bus,
			Manager:    mgr,
			Store:      store,
			Resolver:   resolver,
			DB:         database,
			Scanner:    scanSvc,
			Downloader: dl,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", srv.Addr, err)
	}
	log.Printf("douyin-archive backend listening on http://%s (mock=%v data=%s db=%s sidecar_port=%d)",
		srv.Addr, cfg.Mock, cfg.DataDir, cfg.DBPath, cfg.SidecarPort)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Printf("shutdown signal received")
	}

	// Graceful shutdown: stop accepting, let handlers finish (bounded — SSE
	// connections are closed for real after the grace period), then stop the
	// scheduler and drain in-flight scans (their contexts derive from ctx, so
	// they finalize as failed), and finally sidecar (defer) and database
	// (defer) are released.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Active SSE connections block Shutdown; close them now.
		_ = srv.Close()
	}
	schedWG.Wait()
	scanSvc.WaitIdle(5 * time.Second)
	// Stop the downloader last: scans may enqueue auto-download jobs while
	// finalizing. Stop cancels running transfers (their rows stay
	// downloading so the next RecoverStale requeues them) and waits up to 5s
	// for workers to finalize.
	dl.Stop(downloader.StopGrace)
	return nil
}
