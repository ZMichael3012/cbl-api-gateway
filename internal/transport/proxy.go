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

// NewUpstreamProxy builds a reverse proxy to the upstream with a time budget
// and failure masking: the client receives 503 service_unavailable with
// Retry-After (contract 7.1), error details stay in the perimeter log.
func NewUpstreamProxy(target *url.URL, timeout time.Duration, log *zap.Logger) *httputil.ReverseProxy {
	if log == nil {
		log = zap.NewNop()
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			// SetURL directs the request to the upstream and replaces Host with
			// the upstream's own one: BFF logs and routing do not see the
			// perimeter host. Incoming forwarding headers are removed before
			// SetXForwarded: the client cannot spoof X-Forwarded-For/Host/Proto
			// or Forwarded, the upstream sees only canonical perimeter values.
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
			// If the upstream did not return X-Request-Id, the client receives
			// the perimeter id; when the upstream returns the header no
			// duplicate is created (see proxyHandler).
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
			// The 503 response also carries the request id in the header:
			// proxyHandler removes the preset header to prevent a duplicate.
			writer.Header().Set(ginmw.HeaderRequestID, requestID)
			writer.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(writer).Encode(problem)
		},
	}
	return proxy
}
