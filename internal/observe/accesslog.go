package observe

import (
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"

	"edgeproxy/internal/reqkey"
)

// AccessLog writes one JSON line per finished request, e.g.
//
//	{"msg":"request","method":"GET","path":"/hello","status":200,"duration_ms":1.8,
//	 "bytes":57,"backend":"10.0.0.7:8080","attempts":1,"request_id":"9f2c...",
//	 "client_ip":"203.0.113.9","trace_id":"4bf9..."}
//
// Only the path is logged, never the query string, which often carries tokens.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			// Deferred so a request that panics (an aborted stream, say) is still logged.
			defer func() {
				info := InfoFrom(r.Context())
				attrs := []slog.Attr{
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", rec.status),
					slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
					slog.Int64("bytes", rec.bytes),
					slog.String("backend", info.Backend),
					slog.Int("attempts", info.Attempts),
					slog.String("request_id", info.RequestID),
					slog.String("client_ip", reqkey.ClientIP(r)),
					slog.String("user_agent", r.UserAgent()),
				}
				if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
					attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
				}
				logger.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
