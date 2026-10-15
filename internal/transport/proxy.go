package transport

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"go.uber.org/zap"

	"github.com/cbl/cbl-kit/ginmw"
)

// NewUpstreamProxy собирает reverse proxy к апстриму с бюджетом времени и
// маскировкой сбоев: клиент получает 503 service_unavailable с Retry-After
// (контракт 7.1), детали ошибки остаются в логе периметра.
func NewUpstreamProxy(target *url.URL, timeout time.Duration, log *zap.Logger) *httputil.ReverseProxy {
	if log == nil {
		log = zap.NewNop()
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			// SetURL направляет запрос на апстрим и подменяет Host на его
			// собственный: логи и маршрутизация BFF не видят хост периметра.
			// Входящие forwarding-заголовки удаляются до SetXForwarded: клиент
			// не может подделать X-Forwarded-For/Host/Proto и Forwarded, апстрим
			// видит только канонические значения периметра.
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
			request.Out.Header.Del("X-Real-IP")
			request.Out.Header.Del("Forwarded")
			request.SetURL(target)
			request.SetXForwarded()
		},
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			TLSHandshakeTimeout:   3 * time.Second,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   128,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: timeout,
			ForceAttemptHTTP2:     true,
		},
		ModifyResponse: func(response *http.Response) error {
			// Если апстрим не вернул X-Request-Id, клиент получает id периметра;
			// при возврате заголовка апстримом дубль не создается (см. proxyHandler).
			if response.Header.Get(ginmw.HeaderRequestID) == "" {
				response.Header.Set(ginmw.HeaderRequestID, response.Request.Header.Get(ginmw.HeaderRequestID))
			}
			return nil
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			requestID := request.Header.Get(ginmw.HeaderRequestID)
			log.Warn("upstream request failed",
				zap.String("path", request.URL.Path),
				zap.String("request_id", requestID),
				zap.Error(err))
			problem := ginmw.Problem{
				Type:      "about:blank",
				Title:     "Service Unavailable",
				Status:    http.StatusServiceUnavailable,
				Code:      "service_unavailable",
				Detail:    "сервис временно недоступен",
				RequestID: requestID,
			}
			writer.Header().Set("Content-Type", ginmw.ContentTypeProblem)
			writer.Header().Set("Retry-After", "1")
			// Ответ 503 несет идентификатор запроса и в заголовке: proxyHandler
			// снимает предустановленный заголовок ради защиты от дубля.
			writer.Header().Set(ginmw.HeaderRequestID, requestID)
			writer.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(writer).Encode(problem)
		},
	}
	return proxy
}
