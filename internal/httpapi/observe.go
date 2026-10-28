package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics are the api's Prometheus metrics, on a registry of their own so
// tests can make many servers.
type metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newMetrics() *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "otter_http_requests_total",
			Help: "HTTP requests served, by route pattern and status code.",
		}, []string{"method", "route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "otter_http_request_duration_seconds",
			Help:    "Time to serve an HTTP request, by route pattern.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
	}
	m.registry.MustRegister(m.requests, m.duration,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// MetricsHandler serves /metrics. It belongs on the internal listener
// (OTTER_METRICS_ADDR), never the public one.
func (s *Server) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{})
}

// handleHealth reports whether the platform database answers. The edge
// uses it to decide whether to route here.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		s.log.WarnContext(r.Context(), "health check: platform database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type requestIDKey struct{}

// requestIDHeader links a request's log lines and, later, the operations it
// causes, to JailDeck's audit log (architecture.md §9).
const requestIDHeader = "X-Request-Id"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestID is the id for r: the trusted edge's, if it sent a well-formed
// one, else a new random one. A client's own header is ignored, so it
// can't make its lines look like someone else's.
func (s *Server) requestID(r *http.Request) string {
	if id := r.Header.Get(requestIDHeader); id != "" && validRequestID.MatchString(id) && s.fromTrustedProxy(r) {
		return id
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestID returns the request id in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// observe wraps h with a request id, an access log line and metrics.
func (s *Server) observe(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := s.requestID(r)
		w.Header().Set(requestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		h.ServeHTTP(rec, r)

		// The mux sets Pattern on r as it routes; requests it didn't route
		// (a cross-origin refusal) share one label, keeping cardinality fixed.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		elapsed := time.Since(start)
		s.metrics.requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		s.metrics.duration.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())

		level := slog.LevelInfo
		if route == "GET /healthz" && rec.status == http.StatusOK {
			level = slog.LevelDebug // every few seconds from the edge
		}
		s.log.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route", route),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
			slog.String("client_ip", s.clientIP(r).String()))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// requestIDHandler adds the request id from the context to every log
// record, so all of a request's lines can be found together.
type requestIDHandler struct{ slog.Handler }

func (h requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{h.Handler.WithAttrs(attrs)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{h.Handler.WithGroup(name)}
}
