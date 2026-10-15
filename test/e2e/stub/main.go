// stub - тестовый апстрим для e2e периметра: отвечает 200 JSON с эхом пути,
// идентификатора запроса и признака авторизации; в продуктовый образ не входит.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// response описывает тело ответа stub-апстрима.
type response struct {
	Upstream   string `json:"upstream"`
	Path       string `json:"path"`
	Method     string `json:"method"`
	RequestID  string `json:"request_id"`
	Authorized bool   `json:"authorized"`
	Host       string `json:"host"`
	ReceivedAt string `json:"received_at"`
}

// main поднимает stub-сервер на :9000.
func main() {
	addr := os.Getenv("STUB_ADDR")
	if addr == "" {
		addr = ":9000"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := response{
			Upstream:   "stub",
			Path:       r.URL.Path,
			Method:     r.Method,
			RequestID:  r.Header.Get("X-Request-Id"),
			Authorized: r.Header.Get("Authorization") != "",
			Host:       r.Host,
			ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			log.Printf("encode response: %v", err)
		}
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	fmt.Println("stub upstream started on", addr)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
