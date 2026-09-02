// Command sluice is a Kubernetes-only reverse proxy and ingress controller.
//
// Slice 1 state: static routes from flags, no Kubernetes. The controller is
// wired in at slice 4.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/RamaHerbin/sluice/internal/controller"
	"github.com/RamaHerbin/sluice/internal/model"
	"github.com/RamaHerbin/sluice/internal/observability"
	"github.com/RamaHerbin/sluice/internal/proxy"
	"github.com/RamaHerbin/sluice/internal/router"
)

// Compile-time checks that observability satisfies the consumer interfaces.
var (
	_ proxy.Metrics      = (*observability.Metrics)(nil)
	_ controller.Metrics = (*observability.Metrics)(nil)
	_ proxy.Provider     = (*router.Store)(nil)
	_ controller.Sink    = (*router.Store)(nil)
)

// routeFlags collects repeatable -route host/path=addr[,addr] flags.
type routeFlags []string

func (r *routeFlags) String() string     { return strings.Join(*r, " ") }
func (r *routeFlags) Set(v string) error { *r = append(*r, v); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sluice:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen   = flag.String("listen", ":8080", "proxy listen address")
		admin    = flag.String("admin", ":9090", "admin listen address (/healthz /readyz /metrics)")
		logJSON  = flag.Bool("log-json", false, "log as JSON instead of text")
		logLevel = flag.String("log-level", "info", "log level: debug, info, warn, error")
		routes   routeFlags
	)
	flag.Var(&routes, "route", "static route host/path=addr[,addr] (repeatable)")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("parse -log-level: %w", err)
	}
	log := observability.NewLogger(level, *logJSON)

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	ready := &observability.Readiness{}

	store := router.NewStore()
	snap, err := parseRoutes(routes)
	if err != nil {
		return err
	}
	table, _, err := store.Apply(snap)
	if err != nil {
		return fmt.Errorf("apply static routes: %w", err)
	}
	log.Info("config applied", "version", table.Version, "routes", table.Len())

	handler := proxy.New(store, proxy.Options{Logger: log, Metrics: metrics})

	proxySrv := &http.Server{
		Addr:              *listen,
		Handler:           observability.RequestID(handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	adminSrv := &http.Server{
		Addr:              *admin,
		Handler:           observability.AdminMux(ready, reg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go func() { errc <- serve(adminSrv, log) }()
	go func() { errc <- serve(proxySrv, log) }()

	ready.Set(true)
	metrics.SetReady(true)
	log.Info("sluice started", "listen", *listen, "admin", *admin)

	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-errc:
		return err
	}

	ready.Set(false)
	metrics.SetReady(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var shutdownErr error
	if err := proxySrv.Shutdown(shutdownCtx); err != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown proxy server: %w", err))
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown admin server: %w", err))
	}
	handler.CloseIdleConnections()
	return shutdownErr
}

func serve(srv *http.Server, log *slog.Logger) error {
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s: %w", srv.Addr, err)
	}
	return nil
}

// parseRoutes turns -route flags into a snapshot. Each flag reads
// host/path=addr[,addr]; host may be empty ("/path=addr") to match any host.
func parseRoutes(flags routeFlags) (*model.Snapshot, error) {
	snap := &model.Snapshot{}
	for i, f := range flags {
		target, addrs, ok := strings.Cut(f, "=")
		if !ok || addrs == "" {
			return nil, fmt.Errorf("route %q: want host/path=addr[,addr]", f)
		}
		host, path, ok := strings.Cut(target, "/")
		if !ok {
			return nil, fmt.Errorf("route %q: path must start with /", f)
		}
		path = "/" + path
		key := model.BackendKey{Namespace: "static", Service: fmt.Sprintf("route-%d", i), Port: 0}
		backend := model.Backend{Key: key}
		for _, a := range strings.Split(addrs, ",") {
			backend.Endpoints = append(backend.Endpoints, model.Endpoint{Address: strings.TrimSpace(a), Ready: true})
		}
		snap.Backends = append(snap.Backends, backend)
		snap.Routes = append(snap.Routes, model.Route{
			Key:      model.RouteKey{Namespace: "static", Name: fmt.Sprintf("route-%d", i)},
			Host:     host,
			Path:     path,
			PathType: model.PathPrefix,
			Backend:  key,
			Created:  time.Now(),
		})
	}
	return snap, nil
}
