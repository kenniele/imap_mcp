package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type requestIDKey struct{}

func RequestID(ctx context.Context) string { v, _ := ctx.Value(requestIDKey{}).(string); return v }
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		id := hex.EncodeToString(b)
		w.Header().Set("X-Request-ID", id)
		r.Header.Set("X-Mail-MCP-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type Metrics struct {
	Registry     *prometheus.Registry
	MCPRequests  *prometheus.CounterVec
	MCPDuration  *prometheus.HistogramVec
	IMAPRequests *prometheus.CounterVec
	IMAPDuration *prometheus.HistogramVec
	IMAPErrors   *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	m := &Metrics{Registry: prometheus.NewRegistry(),
		MCPRequests:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "mcp_requests_total", Help: "MCP tool requests."}, []string{"tool", "result"}),
		MCPDuration:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "mcp_request_duration_seconds", Help: "MCP tool latency.", Buckets: prometheus.DefBuckets}, []string{"tool"}),
		IMAPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "imap_requests_total", Help: "IMAP account operations."}, []string{"operation", "result"}),
		IMAPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "imap_request_duration_seconds", Help: "IMAP operation latency.", Buckets: prometheus.DefBuckets}, []string{"operation"}),
		IMAPErrors:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "imap_errors_total", Help: "IMAP errors."}, []string{"operation"})}
	m.Registry.MustRegister(m.MCPRequests, m.MCPDuration, m.IMAPRequests, m.IMAPDuration, m.IMAPErrors)
	return m
}
func (m *Metrics) ObserveIMAP(op string, start time.Time, err error) {
	result := "ok"
	if err != nil {
		result = "error"
		m.IMAPErrors.WithLabelValues(op).Inc()
	}
	m.IMAPRequests.WithLabelValues(op, result).Inc()
	m.IMAPDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

type toolKey struct{}

func WithTool(ctx context.Context, tool string) context.Context {
	return context.WithValue(ctx, toolKey{}, tool)
}
func Tool(ctx context.Context) string { v, _ := ctx.Value(toolKey{}).(string); return v }

func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}
