package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// TestInitializeTracerProvider tests the initialization of a tracer provider.
// Because GITLAB_TRACING parsing is handled by LabKit v2/trace, these tests
// only verify the behaviour of the wrapper itself.
func TestInitializeTracerProvider(t *testing.T) {
	tests := []struct {
		name    string
		setup   func()
		wantErr bool
	}{
		{
			name: "tracing not configured",
			setup: func() {
				_ = os.Setenv(gitlabTracingEnvVar, "")
			},
			wantErr: false,
		},
		{
			name: "otlp http tracer provider",
			setup: func() {
				_ = os.Setenv(gitlabTracingEnvVar, "otlp://localhost:4318")
			},
			wantErr: false,
		},
		{
			name: "otlps (tls) tracer provider",
			setup: func() {
				_ = os.Setenv(gitlabTracingEnvVar, "otlps://collector.example.com:4318")
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			t.Cleanup(func() { _ = os.Unsetenv(gitlabTracingEnvVar) })
			// The wrapper installs the provider globally; put the previous one
			// back so one subtest does not decide what the next one reads.
			previous := otel.GetTracerProvider()
			t.Cleanup(func() { otel.SetTracerProvider(previous) })

			ctx := context.Background()

			closer, err := InitializeTracerProvider(ctx, "gitaly")
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, closer)
			assert.NoError(t, closer.Close())
		})
	}
}

// TestInitializeTracerProviderInstallsGlobalProvider pins the link between the
// provider LabKit builds and the one the gRPC interceptors read. A provider
// that is returned but never installed leaves otel.GetTracerProvider() at the
// noop implementation, so the server accepts GITLAB_TRACING, starts, and
// exports no span at all — which is what the deployment showed.
func TestInitializeTracerProviderInstallsGlobalProvider(t *testing.T) {
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	t.Setenv(gitlabTracingEnvVar, "otlp://localhost:4318")

	closer, err := InitializeTracerProvider(context.Background(), "gitaly")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, closer.Close()) })

	assert.NotEqual(t, trace.NewNoopTracerProvider(), otel.GetTracerProvider(),
		"the provider LabKit built must be the global one the interceptors read")
}

// TestInitializeTracerProviderKeepsTheGlobalProviderWhenDisabled checks the
// other half of the contract: without GITLAB_TRACING the wrapper must not
// touch the global provider, so a process that embeds it keeps whatever
// provider its own tests or host installed.
func TestInitializeTracerProviderKeepsTheGlobalProviderWhenDisabled(t *testing.T) {
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	t.Setenv(gitlabTracingEnvVar, "")

	closer, err := InitializeTracerProvider(context.Background(), "gitaly")
	require.NoError(t, err)
	require.NoError(t, closer.Close())

	assert.Equal(t, previous, otel.GetTracerProvider())
}

// TestInitializeTracerProviderExportsSpans is the end-to-end regression for
// the two reasons the deployment exported no span at all:
//
//  1. the wrapper handed LabKit a non-nil Config whose zero SampleRate means
//     "never sample", so sampler_param=1 in GITLAB_TRACING was ignored;
//  2. otlptracehttp.WithEndpointURL does not append /v1/traces, so a bare
//     otlp://host:port POSTed to "/" and the collector answered 404.
//
// The test runs a real OTLP/HTTP receiver, starts one span, and asserts the
// receiver got a POST on the traces path with a non-empty body.
func TestInitializeTracerProviderExportsSpans(t *testing.T) {
	var requests atomic.Int32
	var gotPath atomic.Value

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	t.Setenv(gitlabTracingEnvVar, "otlp://"+strings.TrimPrefix(receiver.URL, "http://")+"?sampler=probabilistic&sampler_param=1")

	closer, err := InitializeTracerProvider(context.Background(), "gitaly")
	require.NoError(t, err)

	_, span := otel.Tracer("test").Start(context.Background(), "test-span")
	span.End()
	require.NoError(t, closer.Close())

	require.Equal(t, int32(1), requests.Load(), "the exporter must send the sampled span")
	require.Equal(t, "/v1/traces", gotPath.Load(), "a pathless otlp:// endpoint must still reach /v1/traces")
}

// TestInitializeTracerProviderRejectsInvalidEnv makes a typo loud instead of
// silently falling back to a defaults-only provider.
func TestInitializeTracerProviderRejectsInvalidEnv(t *testing.T) {
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	t.Setenv(gitlabTracingEnvVar, "opentracing://collector:6831")

	closer, err := InitializeTracerProvider(context.Background(), "gitaly")
	require.Error(t, err)
	require.NotNil(t, closer)
}
