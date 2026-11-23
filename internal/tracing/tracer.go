package tracing

import (
	"context"
	"fmt"
	"io"
	"os"

	labkitv2trace "gitlab.com/gitlab-org/labkit/v2/trace"
	"go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/contrib/propagators/ot"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// gitlabTracingEnvVar is the environment variable read by LabKit v2/trace to
// configure the tracing backend. Supported format:
//
//	otlp://host:port        HTTP (insecure)
//	otlps://host:port       HTTPS (secure)
//	otlp+tls://host:port    HTTPS (secure)
const gitlabTracingEnvVar = "GITLAB_TRACING"

// defaultSampleRate is LabKit v2/trace's default when it builds the config
// itself. An explicit sampler_param in GITLAB_TRACING overrides it.
const defaultSampleRate = 0.01

// defaultPropagator is the default TextMapPropagator used for context propagation.
// Propagators are objects responsible for propagating tracing data.
// Note: contrary to OpenTracing, OpenTelemetry propagates baggage within contexts, not within spans.
var defaultPropagator = propagation.NewCompositeTextMapPropagator(
	// TraceContext propagator propagates trace IDs across process boundaries.
	propagation.TraceContext{},

	// Baggage propagator propagates baggage across process boundaries.
	propagation.Baggage{},

	// OpenTracing propagator is needed because other components at GitLab still use it.
	ot.OT{},

	// Jaeger/Uber propagator is needed because other components at GitLab still use it.
	jaeger.Jaeger{},
)

// InitializeTracerProvider creates an OpenTelemetry tracer provider using LabKit v2/trace.
// Configuration is read from the GITLAB_TRACING environment variable.
//
// Supported formats:
//
//	otlp://host:port        HTTP (insecure)
//	otlps://host:port       HTTPS (secure)
//	otlp+tls://host:port    HTTPS (secure)
//
// Returns an io.Closer to flush and shut down the provider and an error if
// initialization fails. When GITLAB_TRACING is unset or empty, tracing is
// disabled and a noop closer is returned, preserving the previous behaviour.
func InitializeTracerProvider(ctx context.Context, serviceName string) (io.Closer, error) {
	if os.Getenv(gitlabTracingEnvVar) == "" {
		return io.NopCloser(nil), nil
	}

	// Parse the connection string ourselves instead of handing LabKit a
	// non-nil Config. A non-nil Config with a zero SampleRate means
	// "never sample" (the zero value is deliberate in LabKit's API), so the
	// old wrapper silently dropped every span even when GITLAB_TRACING
	// asked for sampler_param=1. It also lets us add the /v1/traces path:
	// otlptracehttp.WithEndpointURL does not append it, so a bare
	// otlp://host:port would POST to "/" and the collector answers 404.
	env := labkitv2trace.ParseTracingConfigFromEnv()
	if env == nil {
		return io.NopCloser(nil), fmt.Errorf("%s is set but is not a valid OTLP connection string", gitlabTracingEnvVar)
	}

	endpoint := env.Endpoint + env.URLPath
	if env.URLPath == "" {
		endpoint = env.Endpoint + "/v1/traces"
	}

	cfg := &labkitv2trace.Config{
		ServiceName: serviceName,
		Endpoint:    endpoint,
		Insecure:    env.Insecure,
		Headers:     env.Headers,
		// LabKit's default when it builds the Config itself is 1%. Keep that
		// default for a connection string that names no sampler; an explicit
		// sampler_param (including 0) always wins.
		SampleRate: defaultSampleRate,
	}
	if env.ServiceName != "" {
		cfg.ServiceName = env.ServiceName
	}
	if env.SamplerSet {
		cfg.SampleRate = env.SampleRate
	}

	tracer, shutdown, err := labkitv2trace.NewWithConfig(ctx, cfg)
	if err != nil {
		return io.NopCloser(nil), err
	}

	// Install the provider LabKit built as the global one. The gRPC
	// interceptors of the server read otel.GetTracerProvider(), so a provider
	// that is returned but never installed is a server that exports nothing:
	// every interceptor gets the global noop provider and no span is created.
	otel.SetTracerProvider(tracer.Provider())

	// Override the global propagator to include OpenTracing and Jaeger propagators
	// for backward compatibility with other GitLab components that have not yet
	// migrated to W3C TraceContext.
	otel.SetTextMapPropagator(defaultPropagator)

	return &tracerShutdown{shutdown: shutdown}, nil
}

type tracerShutdown struct {
	shutdown func(context.Context) error
}

func (s *tracerShutdown) Close() error {
	return s.shutdown(context.Background())
}
