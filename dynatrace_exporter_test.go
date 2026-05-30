package main

import (
	"bytes"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestGetenvFallsBackToDefault(t *testing.T) {
	t.Setenv("UNSET_KEY", "")
	if got := getenv("UNSET_KEY", "fallback"); got != "fallback" {
		t.Fatalf("want fallback, got %q", got)
	}
}

func TestGetenvUsesValueWhenSet(t *testing.T) {
	t.Setenv("SET_KEY", "real")
	if got := getenv("SET_KEY", "fallback"); got != "real" {
		t.Fatalf("want real, got %q", got)
	}
}

func TestGetenvIntRejectsBadValue(t *testing.T) {
	t.Setenv("PORT_LIKE", "not-a-number")
	if got := getenvInt("PORT_LIKE", 42); got != 42 {
		t.Fatalf("want default 42, got %d", got)
	}
}

func TestGetenvIntRejectsZeroAndNegative(t *testing.T) {
	t.Setenv("PORT_LIKE", "0")
	if got := getenvInt("PORT_LIKE", 8000); got != 8000 {
		t.Fatalf("want default 8000 for zero, got %d", got)
	}
	t.Setenv("PORT_LIKE", "-3")
	if got := getenvInt("PORT_LIKE", 8000); got != 8000 {
		t.Fatalf("want default 8000 for negative, got %d", got)
	}
}

func TestLoadConfigRequiresAPIURL(t *testing.T) {
	t.Setenv("DYNATRACE_API_URL", "")
	t.Setenv("DYNATRACE_API_TOKEN", "token")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error when DYNATRACE_API_URL is empty")
	}
}

func TestLoadConfigRequiresAPIToken(t *testing.T) {
	t.Setenv("DYNATRACE_API_URL", "https://x.example.com/api/v2/metrics/query")
	t.Setenv("DYNATRACE_API_TOKEN", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error when DYNATRACE_API_TOKEN is empty")
	}
}

func TestLoadConfigAppliesOverrides(t *testing.T) {
	t.Setenv("DYNATRACE_API_URL", "https://x.example.com/api/v2/metrics/query")
	t.Setenv("DYNATRACE_API_TOKEN", "dt0c01.dummy")
	t.Setenv("PORT", "9090")
	t.Setenv("METRIC_PREFIX", "dt_")
	t.Setenv("RESOLUTION", "5m")
	t.Setenv("LOOKBACK", "now-1h")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("PORT: got %d", cfg.Port)
	}
	if cfg.MetricPrefix != "dt_" {
		t.Errorf("METRIC_PREFIX: got %q", cfg.MetricPrefix)
	}
	if cfg.Resolution != "5m" {
		t.Errorf("RESOLUTION: got %q", cfg.Resolution)
	}
	if cfg.Lookback != "now-1h" {
		t.Errorf("LOOKBACK: got %q", cfg.Lookback)
	}
}

func TestLoadConfigDefaultsPrefix(t *testing.T) {
	t.Setenv("DYNATRACE_API_URL", "https://x")
	t.Setenv("DYNATRACE_API_TOKEN", "dt0c01.x")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricPrefix != "dynatrace_" {
		t.Errorf("default prefix wrong: %q", cfg.MetricPrefix)
	}
	if cfg.Resolution != "1m" {
		t.Errorf("default resolution wrong: %q", cfg.Resolution)
	}
}

func TestSanitizeMetricName(t *testing.T) {
	cases := map[string]string{
		"builtin:host.cpu.usage":          "host_cpu_usage",
		"builtin:service.response.time":   "service_response_time",
		"ext:custom.app.queue_depth":      "custom_app_queue_depth",
		"builtin:host.disk.usedPct":       "host_disk_usedPct",
		"builtin:apps.web.actionCount":    "apps_web_actionCount",
		"builtin::weird..multiple.dots":   "weird_multiple_dots",
		"":                                "unnamed",
		"...":                             "unnamed",
		"1starts_with_digit":              "1starts_with_digit",
		":leading_colon":                  "leading_colon",
	}
	for in, want := range cases {
		got := sanitizeMetricName(in)
		if got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "__") {
			t.Errorf("sanitize(%q) produced double underscore: %q", in, got)
		}
	}
}

func ptr(f float64) *float64 { return &f }

func TestPickLatestValueSkipsNaN(t *testing.T) {
	nan := math.NaN()
	v, ok := pickLatestValue([]*float64{ptr(1), ptr(2), &nan, &nan})
	if !ok {
		t.Fatal("expected to find a value")
	}
	if v != 2 {
		t.Errorf("want 2, got %v", v)
	}
}

func TestPickLatestValueSkipsNil(t *testing.T) {
	v, ok := pickLatestValue([]*float64{ptr(42.5), nil, nil})
	if !ok || v != 42.5 {
		t.Errorf("want 42.5, got %v ok=%v", v, ok)
	}
}

func TestPickLatestValueReturnsLatest(t *testing.T) {
	v, ok := pickLatestValue([]*float64{ptr(1), ptr(2), ptr(3), ptr(4), ptr(5)})
	if !ok || v != 5 {
		t.Errorf("want 5, got %v ok=%v", v, ok)
	}
}

func TestPickLatestValueAllNaNOrNil(t *testing.T) {
	nan := math.NaN()
	if _, ok := pickLatestValue([]*float64{&nan, nil, &nan}); ok {
		t.Fatal("expected no value for all-NaN-or-nil")
	}
}

func TestPickLatestValueSkipsInf(t *testing.T) {
	inf := math.Inf(1)
	ninf := math.Inf(-1)
	v, ok := pickLatestValue([]*float64{ptr(42), &inf, &ninf})
	if !ok || v != 42 {
		t.Errorf("want 42, got %v ok=%v", v, ok)
	}
}

func TestPickLatestValueZeroIsValid(t *testing.T) {
	v, ok := pickLatestValue([]*float64{ptr(0), nil, nil})
	if !ok || v != 0 {
		t.Errorf("want 0 (a real value), got %v ok=%v", v, ok)
	}
}

func TestPickLatestValueEmpty(t *testing.T) {
	if _, ok := pickLatestValue(nil); ok {
		t.Fatal("expected no value for empty slice")
	}
}

func TestRegistryReturnsPreferred(t *testing.T) {
	r := newRegistry("dynatrace_", prometheus.NewRegistry(), discardLogger())
	r.registerPreferred()
	spec, ok := r.lookup("builtin:host.cpu.usage", 1)
	if !ok {
		t.Fatal("preferred lookup failed")
	}
	if len(spec.labels) != 1 || spec.labels[0] != "host" {
		t.Errorf("preferred labels wrong: %v", spec.labels)
	}
}

func TestRegistryAutoRegistersUnknown(t *testing.T) {
	r := newRegistry("dynatrace_", prometheus.NewRegistry(), discardLogger())
	spec1, ok := r.lookup("builtin:service.response.time", 2)
	if !ok {
		t.Fatal("dynamic registration failed")
	}
	if len(spec1.labels) != 2 || spec1.labels[0] != "dim_0" || spec1.labels[1] != "dim_1" {
		t.Errorf("dynamic labels wrong: %v", spec1.labels)
	}
	spec2, ok := r.lookup("builtin:service.response.time", 2)
	if !ok {
		t.Fatal("second lookup failed")
	}
	if spec1.gauge != spec2.gauge {
		t.Error("dynamic gauge not reused across lookups")
	}
}

func TestRegistryDynamicPrefixApplied(t *testing.T) {
	customReg := prometheus.NewRegistry()
	r := newRegistry("dt_", customReg, discardLogger())
	spec, ok := r.lookup("builtin:custom.test.value", 1)
	if !ok {
		t.Fatal("registration failed")
	}
	spec.gauge.WithLabelValues("entity-1").Set(1.0)
	mfs, err := customReg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "dt_custom_test_value" {
			found = true
			break
		}
	}
	if !found {
		var names []string
		for _, mf := range mfs {
			names = append(names, mf.GetName())
		}
		t.Errorf("metric with prefix not found, got names: %v", names)
	}
}

func TestHandleEarlyFlagsVersion(t *testing.T) {
	var buf bytes.Buffer
	if !handleEarlyFlags([]string{"prog", "--version"}, &buf) {
		t.Error("--version should be handled")
	}
	if !strings.Contains(buf.String(), version) {
		t.Errorf("output should contain version, got %q", buf.String())
	}
	buf.Reset()
	if !handleEarlyFlags([]string{"prog", "-v"}, &buf) {
		t.Error("-v should be handled")
	}
	buf.Reset()
	if !handleEarlyFlags([]string{"prog", "--help"}, &buf) {
		t.Error("--help should be handled")
	}
	if !strings.Contains(buf.String(), "DYNATRACE_API_URL") {
		t.Error("--help output should describe env vars")
	}
	if handleEarlyFlags([]string{"prog"}, io.Discard) {
		t.Error("no flag should not be handled")
	}
	if handleEarlyFlags([]string{"prog", "--bogus"}, io.Discard) {
		t.Error("unknown flag should not be handled")
	}
}
