// Command api-gateway starts the CBL platform HTTP gateway service.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/cbl/cbl-api-gateway/internal/storage"
	"github.com/cbl/cbl-api-gateway/internal/transport"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// rateWindow sets the fixed perimeter rate limit window: counter keys are
// aligned by second per the CBL-003 contract.
const rateWindow = time.Second

// main starts the service and exits the process with a non-zero code on
// initialization failure (the orchestrator must see a failure, not a
// successful exit).
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// run assembles the perimeter dependencies and serves requests until a stop
// signal.
func run() error {
	log, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	httpAddr := getenv("HTTP_ADDR", ":8000")
	metricsAddr := getenv("METRICS_ADDR", ":9200")

	billingURL, err := urlEnv("BILLING_API_URL", "http://localhost:8010")
	if err != nil {
		return err
	}
	backofficeURL, err := urlEnv("BACKOFFICE_URL", "http://localhost:8020")
	if err != nil {
		return err
	}
	limit, err := positiveIntEnv("RATE_LIMIT_PER_SEC", 100)
	if err != nil {
		return err
	}
	upstreamTimeout, err := durationEnv("UPSTREAM_TIMEOUT", 10*time.Second)
	if err != nil {
		return err
	}
	if upstreamTimeout < time.Second || upstreamTimeout > time.Minute {
		return fmt.Errorf("UPSTREAM_TIMEOUT must be between 1s and 60s: %s", upstreamTimeout)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Redis is optional: without it the limits are counted by the local
	// instance limiter (accuracy drops, service does not fail - T-11).
	local := storage.NewLocalLimiter()
	var counter usecase.Counter = local
	var peeker usecase.KeyPeeker
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		store, err := storage.NewRedisStore(addr)
		if err != nil {
			return fmt.Errorf("redis store: %w", err)
		}
		defer func() { _ = store.Close() }()
		if pingErr := store.Ping(ctx); pingErr != nil {
			log.Warn("redis is not reachable, rate limits degrade to local limiter", zap.Error(pingErr))
		}
		counter = usecase.NewFallbackCounter(store, local, log)
		peeker = store
	} else {
		log.Warn("REDIS_ADDR is empty: rate limits are per instance, api key control is delegated to billing api")
	}

	perimeter, err := usecase.NewPerimeter(peeker, counter, log, limit, rateWindow)
	if err != nil {
		return fmt.Errorf("perimeter configuration: %w", err)
	}
	log.Info("perimeter configured",
		zap.Int64("rate_limit_per_sec", limit),
		zap.Duration("window", rateWindow),
		zap.String("billing_api", billingURL.String()),
		zap.String("backoffice", backofficeURL.String()))

	router, err := transport.NewRouter(transport.Config{
		Perimeter:       perimeter,
		BillingAPI:      billingURL,
		Backoffice:      backofficeURL,
		UpstreamTimeout: upstreamTimeout,
		Log:             log,
	})
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}

	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout also covers upstream response streaming, so it is
		// derived from the UPSTREAM_TIMEOUT budget with a margin; otherwise
		// the contract 503/response would not reach the client with large
		// budget values.
		WriteTimeout: upstreamTimeout + 5*time.Second,
		IdleTimeout:  60 * time.Second,
	}
	metricsSrv := &http.Server{Addr: metricsAddr, Handler: metricsHandler(), ReadHeaderTimeout: 5 * time.Second}

	serveErrors := make(chan error, 2)
	go func() {
		log.Info("http server started", zap.String("addr", httpAddr))
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("http server failed", zap.Error(serveErr))
			serveErrors <- fmt.Errorf("http server: %w", serveErr)
			stop()
		}
	}()
	go func() {
		log.Info("metrics server started", zap.String("addr", metricsAddr))
		if serveErr := metricsSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("metrics server failed", zap.Error(serveErr))
			serveErrors <- fmt.Errorf("metrics server: %w", serveErr)
			stop()
		}
	}()

	// A listener failure must terminate the process with a non-zero code
	// (orchestrator restart policy) rather than look like a regular shutdown.
	var serveFailure error
	select {
	case <-ctx.Done():
	case serveFailure = <-serveErrors:
	}
	if serveFailure == nil {
		// Both channels could become ready at the same time: the error must
		// not be lost because of a random select branch choice.
		select {
		case serveFailure = <-serveErrors:
		default:
		}
	}
	log.Info("shutdown started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if shutdownErr := httpSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Error("http shutdown failed", zap.Error(shutdownErr))
	}
	if shutdownErr := metricsSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Error("metrics shutdown failed", zap.Error(shutdownErr))
	}
	log.Info("shutdown finished")
	return serveFailure
}

// metricsHandler serves a metrics placeholder until task 0.7.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# metrics endpoint placeholder (task 0.7)")
	})
	return mux
}

// getenv returns an environment variable or the default value.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// durationEnv parses a duration from the environment; an invalid value is an
// error (fail-safe against a silent configuration typo).
func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

// positiveIntEnv parses a positive integer from the environment; an invalid
// or non-positive value is an error (the limit affects perimeter protection).
func positiveIntEnv(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive: %d", name, parsed)
	}
	return parsed, nil
}

// urlEnv parses an upstream URL from the environment; only http and https
// with a non-empty host and without userinfo, query and fragment are allowed
// (protection from SSRF via configuration and from credential leaks into
// logs).
func urlEnv(name, fallback string) (*url.URL, error) {
	value := getenv(name, fallback)
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%s: scheme must be http or https: %q", name, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%s: host is empty", name)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%s: userinfo is not allowed", name)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fmt.Errorf("%s: query and fragment are not allowed", name)
	}
	return parsed, nil
}
