package otel

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"google.golang.org/grpc"
)

// InitOtelTracing configures opentelemetry exporter and tracer provider for given backend collector.
//func InitOtelTracing(ctx context.Context, configureOtel bool, collectorPort, otelServiceName, backend string, samplingRate *int32) (
func InitOtelTracing(ctx context.Context, configureOtel bool, collectorPort, otelServiceName, backend string) (
	*sdktrace.TracerProvider,
	grpc.UnaryServerInterceptor,
	grpc.StreamServerInterceptor,
	error,
) {
	if !configureOtel {
		return nil, nil, nil, nil
	}
	// Maybe kubelet global TracerProvider is registered?
	var tp *sdktrace.TracerProvider
	var err error
	if len(otelServiceName) == 0 {
		otelServiceName, err = os.Hostname()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(otelServiceName),
	)
	address := fmt.Sprintf("0.0.0.0:%s", collectorPort)
	switch backend {
	case "stdout":
		exporter, err := stdouttrace.New((stdouttrace.WithPrettyPrint()))
		if err != nil {
			return nil, nil, nil, err
		}
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
		)
	case "otlp":
		exporter, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(address),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, nil, nil, err
		}
		// TODO: AlwaysSample for testing, for merge default to
		// Only emit spans when the kubelet sends a request with a sampled trace
		// sampler := sdktrace.NeverSample()
		sampler := sdktrace.AlwaysSample()
		//if samplingRate != nil && *samplingRate > 0 {
		//sampler = sdktrace.TraceIDRatioBased(float64(*samplingRate) / float64(1000000))
		//}
		// batch span processor to aggregate spans before export.
		bsp := sdktrace.NewBatchSpanProcessor(exporter)
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.ParentBased(sampler)),
			sdktrace.WithSpanProcessor(bsp),
			sdktrace.WithResource(res),
		)
	default:
		return nil, nil, nil, fmt.Errorf("OpenTelemetry exporter for backend '%s' not supported.", backend)
	}
	tmp := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	opts := []otelgrpc.Option{otelgrpc.WithPropagators(tmp), otelgrpc.WithTracerProvider(tp)}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(tmp)
	return tp, otelgrpc.UnaryServerInterceptor(opts...), otelgrpc.StreamServerInterceptor(opts...), nil
}
