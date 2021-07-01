package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	otelgo "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpgrpc"
	"go.opentelemetry.io/otel/exporters/trace/jaeger"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv"
	"google.golang.org/grpc"
)

// InitOtelTracing configures opentelemetry exporter and tracer provider for given backend collector.
func InitOtelTracing(ctx context.Context, configureOtel bool, collectorPort, otelServiceName, backend string) (
	*sdktrace.TracerProvider,
	grpc.UnaryServerInterceptor,
	grpc.StreamServerInterceptor,
	error,
) {
	if !configureOtel {
		return nil, nil, nil, nil
	}
	var tp *sdktrace.TracerProvider
	res := resource.NewWithAttributes(
		semconv.ServiceNameKey.String(otelServiceName),
	)
	address := fmt.Sprintf("0.0.0.0:%s", collectorPort)
	switch backend {
	case "otlp":
		exporter, err := otlp.NewExporter(ctx,
			otlpgrpc.NewDriver(
				otlpgrpc.WithEndpoint(address),
				otlpgrpc.WithInsecure(),
			))
		if err != nil {
			return nil, nil, nil, err
		}
		tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	// TODO: Test this path
	case "jaeger":
		exporter, err := jaeger.NewRawExporter(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(address)))
		if err != nil {
			return nil, nil, nil, err
		}
		tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	default:
		return nil, nil, nil, fmt.Errorf("OpenTelemetry exporter for backend '%s' not supported.", backend)
	}
	tmp := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	opts := []otelgrpc.Option{otelgrpc.WithPropagators(tmp), otelgrpc.WithTracerProvider(tp)}
	otelgo.SetTracerProvider(tp)
	otelgo.SetTextMapPropagator(tmp)
	return tp, otelgrpc.UnaryServerInterceptor(opts...), otelgrpc.StreamServerInterceptor(opts...), nil
}
