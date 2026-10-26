package observe

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestRequestIDIsGeneratedWhenMissing(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(RequestIDHeader)
		if InfoFrom(r.Context()).RequestID != seen {
			t.Error("Info.RequestID should match the forwarded header")
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	got := rec.Header().Get(RequestIDHeader)
	if len(got) != 32 || got != seen {
		t.Fatalf("response ID %q, forwarded ID %q; want the same 32-hex-char ID on both", got, seen)
	}
}

func TestRequestIDKeepsValidAndReplacesUnsafeIDs(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for id, keep := range map[string]bool{
		"order-123:retry.2":                    true,
		"has space":                            false,
		`quote"injection`:                      false,
		string(bytes.Repeat([]byte("a"), 129)): false,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(RequestIDHeader, id)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if kept := rec.Header().Get(RequestIDHeader) == id; kept != keep {
			t.Errorf("ID %q: kept = %v, want %v", id, kept, keep)
		}
	}
}

func TestAccessLogWritesOneLinePerRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := RequestID(AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := InfoFrom(r.Context())
		info.Backend, info.Attempts = "10.0.0.7:8080", 2
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	})))
	req := httptest.NewRequest(http.MethodPost, "/orders?token=secret", nil)
	req.Header.Set(RequestIDHeader, "req-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("access log is not one JSON line: %v\n%s", err, buf.String())
	}
	want := map[string]any{
		"msg": "request", "method": "POST", "path": "/orders", "status": float64(201),
		"bytes": float64(5), "backend": "10.0.0.7:8080", "attempts": float64(2), "request_id": "req-1",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if bytes.Contains(buf.Bytes(), []byte("secret")) {
		t.Error("the query string must not be logged")
	}
}

func TestTracingContinuesTheClientsTrace(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	var inner trace.SpanContext
	h := RequestID(Tracing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner = trace.SpanContextFromContext(r.Context())
		w.WriteHeader(http.StatusBadGateway)
	})))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := inner.TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace ID %s, want the client's", got)
	}
	ended := spans.Ended()
	if len(ended) != 1 {
		t.Fatalf("%d spans, want 1 server span", len(ended))
	}
	s := ended[0]
	if s.Parent().SpanID().String() != "00f067aa0ba902b7" || s.SpanKind() != trace.SpanKindServer {
		t.Errorf("server span parent %s kind %v, want the client's span and kind server", s.Parent().SpanID(), s.SpanKind())
	}
	if s.Status().Code.String() != "Error" {
		t.Errorf("a 502 should mark the span as an error, got %v", s.Status())
	}
}
