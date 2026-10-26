package observe

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"edgeproxy/internal/reqkey"
)

// Tracer is used for every span edgeproxy creates. Until SetupTracing runs it
// is a no-op, so tracing costs nothing when disabled.
func Tracer() trace.Tracer { return otel.Tracer("edgeproxy") }

type TracingConfig struct {
	// Endpoint is the OTLP/HTTP collector, e.g. "localhost:4318" (Jaeger, Tempo
	// and the OpenTelemetry Collector all accept it).
	Endpoint string
	// Insecure sends spans over plain HTTP instead of HTTPS.
	Insecure    bool
	SampleRatio float64
	ServiceName string
}

// SetupTracing exports spans to an OTLP collector and turns on W3C Trace
// Context propagation (the traceparent header). The returned function flushes
// any spans still buffered; call it on shutdown.
func SetupTracing(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(time.Second)),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", cfg.ServiceName))),
		// Respect the caller's sampling decision when it sent one; otherwise
		// keep SampleRatio of new traces.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
}

// Tracing starts a server span for each request, continuing the client's
// trace if it sent a traceparent header. The proxy adds a child span per
// backend attempt and passes the trace on to the backend.
func Tracing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := Tracer().Start(ctx, r.Method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("url.path", r.URL.Path),
				attribute.String("client.address", reqkey.ClientIP(r)),
			))
		defer span.End()

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))

		info := InfoFrom(ctx)
		span.SetAttributes(
			attribute.Int("http.response.status_code", rec.status),
			attribute.String("edgeproxy.request_id", info.RequestID),
			attribute.String("edgeproxy.backend", info.Backend),
			attribute.Int("edgeproxy.attempts", info.Attempts),
		)
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
	})
}
