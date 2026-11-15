package telemetry

import (
	"context"
	"fmt"
	"net/http"
	goruntime "runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// meterScope is the instrumentation scope of the service's own metrics.
const meterScope = "go.jacaudi.dev/hephaestus"

// Metrics owns the OpenTelemetry meter provider and the Prometheus registry
// it is exported through. It does not touch any global registry or provider.
type Metrics struct {
	reg *prometheus.Registry
	mp  *sdkmetric.MeterProvider
}

// NewMetrics builds the registry and meter provider, starts Go runtime
// metrics, and registers the hephaestus.build.info gauge.
func NewMetrics(version, commit string) (*Metrics, error) {
	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, fmt.Errorf("prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return nil, fmt.Errorf("runtime metrics: %w", err)
	}

	info := metric.WithAttributes(
		attribute.String("version", version),
		attribute.String("commit", commit),
		attribute.String("go_version", goruntime.Version()),
	)
	_, err = mp.Meter(meterScope).Int64ObservableGauge("hephaestus.build.info",
		metric.WithDescription("Build information; the value is always 1."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, info)
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("build info gauge: %w", err)
	}
	return &Metrics{reg: reg, mp: mp}, nil
}

// Meter returns the meter for the service's own instruments.
func (m *Metrics) Meter() metric.Meter { //nolint:ireturn // OTel API returns the interface
	return m.mp.Meter(meterScope)
}

// Handler serves the registry in Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Instrument wraps h to record request rate, errors and duration under the
// given operation name. serverName is the address of the listener h serves, as
// host:port. It labels the metrics, so the client's Host header never does;
// the port is part of it because otelhttp takes a missing port from that header.
func (m *Metrics) Instrument(h http.Handler, operation, serverName string) http.Handler {
	return otelhttp.NewHandler(h, operation, otelhttp.WithMeterProvider(m.mp), otelhttp.WithServerName(serverName))
}

// Shutdown stops the meter provider.
func (m *Metrics) Shutdown(ctx context.Context) error {
	if err := m.mp.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown meter provider: %w", err)
	}
	return nil
}
