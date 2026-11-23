package tracing

import (
	"context"
	"os"
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
