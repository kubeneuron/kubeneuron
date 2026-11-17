// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// receiver collects OTLP export requests.
type receiver struct {
	colmetrics.UnimplementedMetricsServiceServer
	mu   sync.Mutex
	reqs []*colmetrics.ExportMetricsServiceRequest
	got  chan struct{}
}

func newReceiver() *receiver { return &receiver{got: make(chan struct{}, 64)} }

func (r *receiver) Export(_ context.Context, req *colmetrics.ExportMetricsServiceRequest) (*colmetrics.ExportMetricsServiceResponse, error) {
	r.add(req)
	return &colmetrics.ExportMetricsServiceResponse{}, nil
}

func (r *receiver) add(req *colmetrics.ExportMetricsServiceRequest) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	select {
	case r.got <- struct{}{}:
	default:
	}
}

func (r *receiver) wait(t *testing.T) *colmetrics.ExportMetricsServiceRequest {
	t.Helper()
	select {
	case <-r.got:
	case <-time.After(10 * time.Second):
		t.Fatal("no OTLP export arrived")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[len(r.reqs)-1]
}

func testRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	signals := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubeneuron_signals_total", Help: "test",
	}, []string{"source", "class"})
	signals.WithLabelValues("agent", "xid").Add(3)
	reg.MustRegister(signals)
	return reg
}

func attrs(kv []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, a := range kv {
		out[a.GetKey()] = a.GetValue().GetStringValue()
	}
	return out
}

// exported returns the resource attributes and, per metric name, the
// attributes of its first data point.
func exported(req *colmetrics.ExportMetricsServiceRequest) (map[string]string, map[string]map[string]string) {
	res := map[string]string{}
	metrics := map[string]map[string]string{}
	for _, rm := range req.GetResourceMetrics() {
		for k, v := range attrs(rm.GetResource().GetAttributes()) {
			res[k] = v
		}
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				var point map[string]string
				if s := m.GetSum(); s != nil && len(s.GetDataPoints()) > 0 {
					point = attrs(s.GetDataPoints()[0].GetAttributes())
				}
				metrics[m.GetName()] = point
			}
		}
	}
	return res, metrics
}

func fastExport(t *testing.T, endpoint string) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "50")
}

func TestOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	reg := testRegistry()
	stop := Start(context.Background(), Options{ServiceName: "kubeneuron-test", Gatherer: reg, Registerer: reg})
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := testutil.GatherAndCount(reg, "kubeneuron_telemetry_export_failures_total"); n != 0 {
		t.Fatalf("failure counter registered while export is off")
	}
}

func TestConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		enabled  bool
		protocol string
		invalid  bool
	}{
		{name: "no endpoint", env: map[string]string{}},
		{name: "endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4317"}, enabled: true, protocol: "grpc"},
		{name: "metrics endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://c:4318/v1/metrics", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"}, enabled: true, protocol: "http/protobuf"},
		{name: "metrics protocol wins", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"}, enabled: true, protocol: "grpc"},
		{name: "sdk disabled", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c", "OTEL_SDK_DISABLED": "TRUE"}},
		{name: "exporter none", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c", "OTEL_METRICS_EXPORTER": "none"}},
		{name: "exporter prometheus", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c", "OTEL_METRICS_EXPORTER": "prometheus"}, invalid: true},
		{name: "http/json", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled, protocol, err := configured(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.invalid || enabled != tc.enabled || protocol != tc.protocol {
				t.Fatalf("configured = %v %q %v, want %v %q invalid=%v", enabled, protocol, err, tc.enabled, tc.protocol, tc.invalid)
			}
		})
	}
}

func TestInvalidConfigurationRunsWithoutExport(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_METRICS_EXPORTER", "console")
	reg := testRegistry()
	stop := Start(context.Background(), Options{Gatherer: reg, Registerer: reg})
	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := testutil.GatherAndCount(reg, "kubeneuron_telemetry_export_failures_total"); n != 0 {
		t.Fatalf("export started despite an invalid exporter")
	}
}

func TestGRPCExportCarriesRegistrySeries(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	rcv := newReceiver()
	colmetrics.RegisterMetricsServiceServer(srv, rcv)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	fastExport(t, "http://"+lis.Addr().String())
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "k8s.cluster.name=c1,k8s.namespace.name=kubeneuron-system")
	reg := testRegistry()
	stop := Start(context.Background(), Options{ServiceName: "kubeneuron-test", Version: "v9", Gatherer: reg, Registerer: reg})
	defer func() { _ = stop(context.Background()) }()

	res, metrics := exported(rcv.wait(t))
	for k, want := range map[string]string{
		"service.name":       "kubeneuron-test",
		"service.version":    "v9",
		"k8s.cluster.name":   "c1",
		"k8s.namespace.name": "kubeneuron-system",
	} {
		if res[k] != want {
			t.Errorf("resource %s = %q, want %q (all: %v)", k, res[k], want, res)
		}
	}
	point, ok := metrics["kubeneuron_signals_total"]
	if !ok {
		t.Fatalf("kubeneuron_signals_total not exported under its Prometheus name; got %v", keys(metrics))
	}
	if point["source"] != "agent" || point["class"] != "xid" || len(point) != 2 {
		t.Fatalf("labels = %v, want exactly source=agent class=xid", point)
	}
	if _, ok := metrics["kubeneuron_telemetry_export_failures_total"]; !ok {
		t.Errorf("failure counter is not exported with the rest of the registry")
	}
}

func TestServiceNameFromEnvironmentOverrides(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	rcv := newReceiver()
	colmetrics.RegisterMetricsServiceServer(srv, rcv)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	fastExport(t, "http://"+lis.Addr().String())
	t.Setenv("OTEL_SERVICE_NAME", "renamed")
	reg := testRegistry()
	stop := Start(context.Background(), Options{ServiceName: "kubeneuron-test", Gatherer: reg, Registerer: reg})
	defer func() { _ = stop(context.Background()) }()
	if res, _ := exported(rcv.wait(t)); res["service.name"] != "renamed" {
		t.Fatalf("service.name = %q, want OTEL_SERVICE_NAME", res["service.name"])
	}
}

func TestHTTPProtobufExport(t *testing.T) {
	rcv := newReceiver()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := &colmetrics.ExportMetricsServiceRequest{}
		if r.URL.Path != "/v1/metrics" || proto.Unmarshal(body, req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		rcv.add(req)
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&colmetrics.ExportMetricsServiceResponse{})
		_, _ = w.Write(out)
	}))
	defer srv.Close()

	fastExport(t, srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	reg := testRegistry()
	stop := Start(context.Background(), Options{ServiceName: "kubeneuron-test", Gatherer: reg, Registerer: reg})
	defer func() { _ = stop(context.Background()) }()
	if _, metrics := exported(rcv.wait(t)); metrics["kubeneuron_signals_total"] == nil {
		t.Fatalf("kubeneuron_signals_total missing over http/protobuf; got %v", keys(metrics))
	}
}

// An unreachable Collector costs the component nothing but a counter: Start
// returns at once, failures are counted, and shutdown stays bounded.
func TestUnreachableCollectorNeverBlocks(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	fastExport(t, "http://"+addr)
	t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "200")
	reg := testRegistry()
	began := time.Now()
	stop := Start(context.Background(), Options{ServiceName: "kubeneuron-test", Gatherer: reg, Registerer: reg})
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("Start took %v with the Collector down", d)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := testutil.GatherAndCount(reg, "kubeneuron_telemetry_export_failures_total")
		if err != nil {
			t.Fatal(err)
		}
		if n == 1 && failures(t, reg) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed exports were not counted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	began = time.Now()
	_ = stop(context.Background())
	if d := time.Since(began); d > ShutdownTimeout+time.Second {
		t.Fatalf("shutdown took %v, want at most %v", d, ShutdownTimeout)
	}
}

func failures(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "kubeneuron_telemetry_export_failures_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

func keys(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
