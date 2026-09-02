// Command sluice is a Kubernetes-only reverse proxy and ingress controller.
//
// It watches Ingress, IngressClass, Service and EndpointSlice objects,
// compiles them into an immutable routing table and serves HTTP with it.
// There is no configuration file: Kubernetes is the only configuration
// source (ADR 0006). Startup order: admin server, informers, initial sync,
// readiness, proxy server. Shutdown order: readiness off, servers drained,
// informers stopped, idle upstream connections closed.
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
	"sync"
	"syscall"
	"time"

	"github.com/RamaHerbin/sluice/internal/controller"
	"github.com/RamaHerbin/sluice/internal/observability"
	"github.com/RamaHerbin/sluice/internal/proxy"
	"github.com/RamaHerbin/sluice/internal/router"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// Compile-time checks that the concrete types satisfy the consumer interfaces.
var (
	_ proxy.Metrics      = (*observability.Metrics)(nil)
	_ controller.Metrics = (*observability.Metrics)(nil)
	_ proxy.Provider     = (*router.Store)(nil)
	_ controller.Sink    = (*router.Store)(nil)
)

const shutdownTimeout = 20 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sluice:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen       = flag.String("listen", ":8080", "proxy listen address")
		admin        = flag.String("admin", ":9090", "admin listen address (/healthz /readyz /metrics)")
		kubeconfig   = flag.String("kubeconfig", "", "path to a kubeconfig file; empty means in-cluster configuration")
		ingressClass = flag.String("ingress-class", controller.DefaultIngressClass, "name of the IngressClass to claim")
		namespace    = flag.String("namespace", "", "watch only this namespace instead of the whole cluster")
		logJSON      = flag.Bool("log-json", false, "log as JSON instead of text")
		logLevel     = flag.String("log-level", "info", "log level: debug, info, warn, error")
		showVersion  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("parse -log-level: %w", err)
	}
	log := observability.NewLogger(level, *logJSON)
	metrics := observability.NewMetrics()
	ready := &observability.Readiness{}
	store := router.NewStore()

	client, err := controller.NewClientset(*kubeconfig)
	if err != nil {
		return fmt.Errorf("connect to kubernetes: %w", err)
	}
	ctrl := controller.New(client, store, log, metrics, controller.Options{
		IngressClass: *ingressClass,
		Namespace:    *namespace,
	})
	handler := proxy.New(store, proxy.Options{Logger: log, Metrics: metrics})

	proxySrv := &http.Server{
		Addr:              *listen,
		Handler:           observability.RequestID(handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	adminSrv := &http.Server{
		Addr:              *admin,
		Handler:           observability.AdminMux(ready, metrics),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Signals end the serving phase. The controller has its own context so
	// informers keep running while the servers drain and stop only after.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	ctrlCtx, stopController := context.WithCancel(context.Background())
	defer stopController()

	errc := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ctrl.Run(ctrlCtx); err != nil {
			errc <- fmt.Errorf("controller: %w", err)
		}
	}()
	go func() { errc <- serve(adminSrv, log) }()
	log.Info("sluice starting", "version", version, "ingress_class", *ingressClass, "namespace", *namespace)

	var runErr error
	select {
	case <-ctrl.Synced():
		table := store.Current()
		ready.Set(true)
		metrics.SetReady(true)
		log.Info("initial sync complete, serving", "version", table.Version, "routes", table.Len(), "listen", *listen, "admin", *admin)
		go func() { errc <- serve(proxySrv, log) }()
		select {
		case <-signalCtx.Done():
			log.Info("shutdown requested")
		case runErr = <-errc:
			log.Error("component failed, shutting down", "error", runErr)
		}
	case <-signalCtx.Done():
		log.Info("shutdown requested before initial sync")
	case runErr = <-errc:
		log.Error("startup failed", "error", runErr)
	}

	ready.Set(false)
	metrics.SetReady(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	var shutdownErr error
	if err := proxySrv.Shutdown(shutdownCtx); err != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown proxy server: %w", err))
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown admin server: %w", err))
	}
	stopController()
	wg.Wait()
	handler.CloseIdleConnections()
	log.Info("sluice stopped")
	return errors.Join(runErr, shutdownErr)
}

func serve(srv *http.Server, log *slog.Logger) error {
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s: %w", srv.Addr, err)
	}
	return nil
}
