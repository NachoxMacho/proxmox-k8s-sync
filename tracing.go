package main

import (
	"fmt"
	"log/slog"
	"strings"

	"context"
	"runtime"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

func InitializeTracer(traceAddr string) (context.Context, func(context.Context) error, error) {
	serviceName := semconv.ServiceNameKey.String("proxmox-k8s-sync")
	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			// The service name used to display traces in backends
			serviceName,
		),
	)
	if err != nil {
		return nil, nil, err
	}


	// Set up a trace exporter
	traceExporter, err := otlptracehttp.New(
		ctx,
		otlptracehttp.WithEndpointURL(traceAddr),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	// Register the trace exporter with a TracerProvider, using a batch
	// span processor to aggregate spans before export.
	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	limits := sdktrace.NewSpanLimits()
	limits.EventCountLimit = -1
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithRawSpanLimits(limits),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(tracerProvider)

	// Set global propagator to tracecontext (the default is no-op).
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return ctx, tracerProvider.Shutdown, nil
}

// TraceError w ill set the span to an error status, and log the error to the span.
func TraceError(ctx context.Context, span trace.Span, err error) error {
	slog.ErrorContext(ctx, "error encounterd",
		slog.String("error", err.Error()),
		slog.String("trace_id", span.SpanContext().TraceID().String()),
		slog.String("span_id", span.SpanContext().SpanID().String()),
	)
	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
	return err
}

// StartTrace if name is empty, it will use the function name to identify the trace.
func StartTrace(
	ctx context.Context,
	name string,
	options ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	// This maps to the Library Name in the span
	tracer := otel.Tracer("proxmox-k8s-sync")
	funcName := callerName(1)
	options = append(options, trace.WithAttributes(attribute.String("function", funcName)))

	if name == "" {
		shortNameSplit := strings.Split(funcName, "/")
		name = shortNameSplit[len(shortNameSplit)-1]
	}
	ctx, span := tracer.Start( //nolint:spancheck // This is not needed as we are returning the span
		ctx,
		name,
		options...)
	return ctx, span //nolint:spancheck // This is not needed as we are returning the span
}

func callerName(skip int) string {
	pc, _, _, ok := runtime.Caller(skip + 1)
	if !ok {
		return ""
	}
	f := runtime.FuncForPC(pc)
	if f == nil {
		return ""
	}
	return f.Name()
}

