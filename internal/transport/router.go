package transport

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/cbl/cbl-kit/ginmw"

	"github.com/cbl/cbl-api-gateway/internal/transport/httpmw"
	"github.com/cbl/cbl-api-gateway/internal/usecase"
)

// Config описывает зависимости сборки маршрутов периметра.
type Config struct {
	// Perimeter применяет контроль ключей и лимиты на маршрутах /v1.
	Perimeter *usecase.Perimeter
	// BillingAPI задает апстрим мерчант API.
	BillingAPI *url.URL
	// Backoffice задает апстрим операторского API.
	Backoffice *url.URL
	// UpstreamTimeout задает бюджет ожидания ответа апстрима.
	UpstreamTimeout time.Duration
	// Log задает логгер сервиса.
	Log *zap.Logger
}

// NewRouter собирает HTTP-маршрутизацию периметра: /v1 с контролем ключа и
// лимитом, /admin/v1 без лимита мерчанта (RBAC - 12.x), остальное - 404
// problem+json; сбои апстримов отдаются как 503 (контракт 7.1).
func NewRouter(cfg Config) (*gin.Engine, error) {
	if cfg.Perimeter == nil {
		return nil, errors.New("perimeter is required")
	}
	if cfg.BillingAPI == nil || cfg.Backoffice == nil {
		return nil, errors.New("upstream urls are required")
	}
	if cfg.UpstreamTimeout <= 0 {
		return nil, errors.New("upstream timeout must be positive")
	}
	if cfg.Log == nil {
		cfg.Log = zap.NewNop()
	}

	router := gin.New()
	// Редиректы отключены: 301 на POST теряет тело и ломает контракт;
	// нормализацию путей делает апстрим-контракт, а не периметр.
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.Use(
		httpmw.SanitizeRequestID(),
		ginmw.RequestID(),
		ginmw.AccessLog(cfg.Log),
		ginmw.Recovery(cfg.Log),
		httpmw.ForwardRequestID(),
	)
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	billingProxy := NewUpstreamProxy(cfg.BillingAPI, cfg.UpstreamTimeout, cfg.Log)
	backofficeProxy := NewUpstreamProxy(cfg.Backoffice, cfg.UpstreamTimeout, cfg.Log)

	v1 := router.Group("/v1", httpmw.KeyPeek(cfg.Perimeter), httpmw.RateLimit(cfg.Perimeter))
	v1.Any("", proxyHandler(billingProxy))
	v1.Any("/*path", proxyHandler(billingProxy))

	admin := router.Group("/admin/v1")
	admin.Any("", proxyHandler(backofficeProxy))
	admin.Any("/*path", proxyHandler(backofficeProxy))

	router.NoRoute(func(c *gin.Context) {
		ginmw.AbortWithProblem(c, http.StatusNotFound, "not_found", "Not Found", "")
	})
	return router, nil
}

// proxyHandler адаптирует reverse proxy к обработчику Gin: обертка скрывает
// необязательные интерфейсы gin (его CloseNotify паникует на несрастимом
// Writer, например httptest.Recorder); отмена запроса отслеживается контекстом
// запроса, который сервер отменяет при обрыве соединения клиента. Заголовок
// X-Request-Id, заранее выставленный ginmw, снимается: заголовок ответа
// апстрима добавляется через Header.Add и без снятия дал бы дубль.
func proxyHandler(proxy *httputil.ReverseProxy) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Del(ginmw.HeaderRequestID)
		proxy.ServeHTTP(proxyWriter{ResponseWriter: c.Writer}, c.Request)
	}
}

// proxyWriter прячет CloseNotify gin от ReverseProxy и пробрасывает Flush для
// потоковых ответов.
type proxyWriter struct {
	http.ResponseWriter
}

// Flush отправляет накопленный буфер, если нижележащий Writer это умеет.
func (w proxyWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
