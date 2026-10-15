// main запускает HTTP-сервис api-gateway платформы CBL.
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

// rateWindow задает фиксированное окно лимита периметра: ключи счетчиков
// выровнены по секунде контракта CBL-003.
const rateWindow = time.Second

// main запускает сервис и завершает процесс ненулевым кодом при ошибке
// инициализации (оркестратор должен видеть отказ, а не успешный выход).
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// run собирает зависимости периметра и обслуживает запросы до сигнала остановки.
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

	// Redis опционален: без него лимиты считает локальный лимитер инстанса
	// (точность падает, обслуживание не отказывает - T-11).
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
		// WriteTimeout покрывает и стриминг ответа апстрима, поэтому выводится
		// из бюджета UPSTREAM_TIMEOUT с запасом, иначе контрактный 503/ответ не
		// дойдет до клиента при больших значениях бюджета.
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

	// Сбой листенера обязан завершить процесс ненулевым кодом (restart-политика
	// оркестратора), а не выглядеть штатной остановкой.
	var serveFailure error
	select {
	case <-ctx.Done():
	case serveFailure = <-serveErrors:
	}
	if serveFailure == nil {
		// Оба канала могли стать готовы одновременно: ошибка не должна
		// потеряться из-за случайного выбора ветки select.
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

// metricsHandler отдает заглушку метрик до задачи 0.7.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# metrics endpoint placeholder (task 0.7)")
	})
	return mux
}

// getenv возвращает переменную окружения или значение по умолчанию.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// durationEnv разбирает duration из окружения; невалидное значение - ошибка
// (fail-safe против молчаливой опечатки в конфигурации).
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

// positiveIntEnv разбирает положительное целое из окружения; невалидное или
// неположительное значение - ошибка (лимит влияет на защиту периметра).
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

// urlEnv разбирает URL апстрима из окружения; допускаются только http и https
// с непустым хостом без userinfo, query и fragment (защита от SSRF через
// конфигурацию и утечки креденшелов в логи).
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
