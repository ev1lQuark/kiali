// The observability package provides utilities for the Kiali server
// to instrument itself with observability tools such as tracing to provide
// better insights into server performance.
package observability

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/log"
)

const (
	// TracingService is the name of the kiali tracer service.
	TracingService = "kiali"
)

const (
	HTTP  = "http"
	HTTPS = "https"
	GRPC  = "grpc"
)

const (
	JAEGER = "jaeger"
	OTEL   = "otel"
)

// EndFunc ends a span if one is started. Otherwise does nothing.
type EndFunc func()

// TracerName is the name of the global kiali Trace.
func TracerName() string {
	return TracingService + "." + config.Get().Deployment.Namespace
}

// InitTracer initalizes a TracerProvider that exports to jaeger.
// This will panic if there's an error in setup.

func InitTracer(collectorURL string) *sdktrace.TracerProvider {

	exporter, err := getExporter(collectorURL)

	if err != nil {
		log.Errorf("Failed to initialize tracer. Kiali will not log its own tracing data: %v", err)
		return nil
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(config.Get().Server.Observability.Tracing.SamplingRate))),
		sdktrace.WithBatcher(exporter),
		// Record information about this application in an Resource.
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(TracerName()),
			semconv.ServiceNamespaceKey.String(config.Get().Deployment.Namespace),
			// In order for kiali to dog food its own traces, this attribute is set. When determining if an app's
			// traces match its workload, the business logic will parse this hostname attribute.
			attribute.String("hostname", TracerName()),
			attribute.String("instance_name", config.Get().Deployment.InstanceName),
		)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp
}

// Stop shutdown the provider.
func StopTracer(provider *sdktrace.TracerProvider) {
	if provider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		_ = provider.Shutdown(ctx)
	}
}

// Attribute transforms any k/v into an attribute.KeyValue.
// val types that are not recognized return an empty Value.
func Attribute(key string, val interface{}) attribute.KeyValue {
	var kv attribute.KeyValue
	switch v := val.(type) {
	case string:
		kv = attribute.String(key, v)
	case bool:
		kv = attribute.Bool(key, v)
	case int:
		kv = attribute.Int(key, v)
	case int64:
		kv = attribute.Int64(key, v)
	case float64:
		kv = attribute.Float64(key, v)
	case []string:
		kv = attribute.StringSlice(key, v)
	case []bool:
		kv = attribute.BoolSlice(key, v)
	case []int:
		kv = attribute.IntSlice(key, v)
	case []int64:
		kv = attribute.Int64Slice(key, v)
	default:
		// Check for stringer
		if v, ok := val.(fmt.Stringer); ok {
			kv = attribute.Stringer(key, v)
		}
	}

	return kv
}

// StartSpan creates and starts a span from the given context. It returns
// a new context with the span added and a func to be called when the span ends.
// If tracing is not enabled, this function does nothing. The return func is
// safe to call even when tracing is not enabled.
func StartSpan(ctx context.Context, funcName string, attrs ...attribute.KeyValue) (context.Context, EndFunc) {
	var span trace.Span
	if config.Get().Server.Observability.Tracing.Enabled {
		ctx, span = otel.Tracer(TracerName()).Start(ctx, funcName,
			trace.WithAttributes(attrs...),
		)
		return ctx, func() { span.End() }
	}

	return ctx, func() {}
}

// getExporter returns the exporter based on the configuration options
// Tracing collector, OpenTelemetry using http or grpc
func getExporter(collectorURL string) (sdktrace.SpanExporter, error) {
	var exporter sdktrace.SpanExporter
	var err error

	tracingOpt := config.Get().Server.Observability.Tracing

	// Tracing collector
	if tracingOpt.CollectorType == JAEGER {
		log.Debugf("Creating Tracing collector with URL %s", collectorURL)
		exporter, err = jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(collectorURL)))
	} else {
		if tracingOpt.CollectorType == OTEL {
			// OpenTelemetry collector
			if tracingOpt.Otel.Protocol == HTTP || tracingOpt.Otel.Protocol == HTTPS {
				tracingOptions := otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
					Enabled:         true,
					InitialInterval: 1 * time.Nanosecond,
					MaxInterval:     1 * time.Nanosecond,
					// Never stop retry of retry-able status.
					MaxElapsedTime: 0,
				})
				var client otlptrace.Client

				if tracingOpt.Otel.Protocol == HTTP {
					log.Debugf("Creating OpenTelemetry collector with URL http://%s", collectorURL)

					client = otlptracehttp.NewClient(otlptracehttp.WithEndpoint(collectorURL),
						otlptracehttp.WithInsecure(),
						tracingOptions,
					)
				} else {
					log.Debugf("Creating OpenTelemetry collector with URL https://%s", collectorURL)
					// That's mainly for testing
					if tracingOpt.Otel.SkipVerify {
						log.Trace("OpenTelemetry collector will not verify the remote certificate")
						client = otlptracehttp.NewClient(otlptracehttp.WithEndpoint(collectorURL),
							otlptracehttp.WithTLSClientConfig(&tls.Config{InsecureSkipVerify: true}),
							tracingOptions,
						)
					} else {
						client = otlptracehttp.NewClient(otlptracehttp.WithEndpoint(collectorURL),
							tracingOptions,
						)
					}
				}
				ctx := context.Background()
				httpExporter, err2 := otlptrace.New(ctx, client)
				return httpExporter, err2
			} else {
				if tracingOpt.Otel.Protocol == GRPC {
					log.Debugf("Creating OpenTelemetry grpc collector with URL %s", collectorURL)
					ctx := context.Background()
					ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()

					opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(collectorURL), otlptracegrpc.WithDialOption(grpc.WithBlock())}

					if tracingOpt.Otel.TLSEnabled {

						if tracingOpt.Otel.SkipVerify {
							log.Trace("OpenTelemetry collector will not verify the remote certificate")
							tlsConfig := &tls.Config{
								InsecureSkipVerify: true,
							}
							opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig)))
						} else {
							certName := tracingOpt.Otel.CAName
							if certName == "" {
								return nil, fmt.Errorf("ca_name is required")
							}
							creds, errorTLS := credentials.NewClientTLSFromFile(certName, "")
							if errorTLS != nil {
								log.Fatalf("Error loading certificate: %s", errorTLS)
								return nil, errorTLS
							}
							opts = append(opts, otlptracegrpc.WithTLSCredentials(creds))
						}
					} else {
						opts = append(opts, otlptracegrpc.WithInsecure())
					}
					exporter, err = otlptracegrpc.New(ctx, opts...)
				}
			}
		}
	}
	return exporter, err
}
