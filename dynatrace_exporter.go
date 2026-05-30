package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var version = "dev"

type config struct {
	APIURL         string
	APIToken       string
	Port           int
	ScrapeInterval time.Duration
	HTTPTimeout    time.Duration
	MetricSelector string
	EntitySelector string
	MetricPrefix   string
	Resolution     string
	Lookback       string
	MaxRetries     int
	HealthyAfter   time.Duration
}

const defaultMetricSelector = "builtin:host.cpu.usage," +
	"builtin:host.cpu.idle," +
	"builtin:host.mem.usage," +
	"builtin:host.disk.avail," +
	"builtin:host.disk.usedPct," +
	"builtin:host.net.bytesRx," +
	"builtin:host.net.bytesTx"

func loadConfig() (*config, error) {
	c := &config{
		APIURL:         os.Getenv("DYNATRACE_API_URL"),
		APIToken:       os.Getenv("DYNATRACE_API_TOKEN"),
		Port:           getenvInt("PORT", 8000),
		ScrapeInterval: time.Duration(getenvInt("SCRAPE_INTERVAL_SEC", 60)) * time.Second,
		HTTPTimeout:    time.Duration(getenvInt("HTTP_TIMEOUT_SEC", 10)) * time.Second,
		MetricSelector: getenv("METRIC_SELECTOR", defaultMetricSelector),
		EntitySelector: os.Getenv("ENTITY_SELECTOR"),
		MetricPrefix:   getenv("METRIC_PREFIX", "dynatrace_"),
		Resolution:     getenv("RESOLUTION", "1m"),
		Lookback:       getenv("LOOKBACK", "now-10m"),
		MaxRetries:     getenvInt("MAX_RETRIES", 3),
		HealthyAfter:   time.Duration(getenvInt("HEALTHY_AFTER_SEC", 180)) * time.Second,
	}
	if c.APIURL == "" {
		return nil, errors.New("DYNATRACE_API_URL is required")
	}
	if c.APIToken == "" {
		return nil, errors.New("DYNATRACE_API_TOKEN is required")
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

type metricResponse struct {
	Result []struct {
		MetricID string `json:"metricId"`
		Data     []struct {
			Dimensions []string   `json:"dimensions"`
			Values     []*float64 `json:"values"`
		} `json:"data"`
	} `json:"result"`
}

type metricSpec struct {
	gauge  *prometheus.GaugeVec
	labels []string
}

func newPreferredMetrics() map[string]metricSpec {
	return map[string]metricSpec{
		"builtin:host.cpu.usage": {
			gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "dynatrace_host_cpu_usage_percent",
				Help: "Host CPU usage percentage from Dynatrace builtin:host.cpu.usage.",
			}, []string{"host"}),
			labels: []string{"host"},
		},
		"builtin:host.cpu.idle": {
			gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "dynatrace_host_cpu_idle_percent",
				Help: "Host CPU idle percentage from Dynatrace builtin:host.cpu.idle.",
			}, []string{"host"}),
			labels: []string{"host"},
		},
		"builtin:host.mem.usage": {
			gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "dynatrace_host_memory_usage_percent",
				Help: "Host memory usage percentage from Dynatrace builtin:host.mem.usage.",
			}, []string{"host"}),
			labels: []string{"host"},
		},
		"builtin:host.swap.used": {
			gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "dynatrace_host_swap_used_bytes",
				Help: "Host swap usage in bytes from Dynatrace builtin:host.swap.used.",
			}, []string{"host"}),
			labels: []string{"host"},
		},
		"builtin:host.disk.avail": {
			gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "dynatrace_host_disk_available_bytes",
				Help: "Host disk available bytes from Dynatrace builtin:host.disk.avail.",
			}, []string{"host", "disk"}),
			labels: []string{"host", "disk"},
		},
		"builtin:host.disk.usedPct": {
			gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "dynatrace_host_disk_used_percent",
				Help: "Host disk used percentage from Dynatrace builtin:host.disk.usedPct.",
			}, []string{"host", "disk"}),
			labels: []string{"host", "disk"},
		},
		"builtin:host.net.bytesRx": {
			gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "dynatrace_host_network_rx_bytes",
				Help: "Host network bytes received from Dynatrace builtin:host.net.bytesRx.",
			}, []string{"host"}),
			labels: []string{"host"},
		},
		"builtin:host.net.bytesTx": {
			gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
				Name: "dynatrace_host_network_tx_bytes",
				Help: "Host network bytes transmitted from Dynatrace builtin:host.net.bytesTx.",
			}, []string{"host"}),
			labels: []string{"host"},
		},
	}
}

type registry struct {
	preferred map[string]metricSpec
	mu        sync.Mutex
	dynamic   map[string]metricSpec
	prefix    string
	registrar prometheus.Registerer
	logger    *slog.Logger
}

func newRegistry(prefix string, registrar prometheus.Registerer, logger *slog.Logger) *registry {
	return &registry{
		preferred: newPreferredMetrics(),
		dynamic:   map[string]metricSpec{},
		prefix:    prefix,
		registrar: registrar,
		logger:    logger,
	}
}

func (r *registry) registerPreferred() {
	for _, spec := range r.preferred {
		r.registrar.MustRegister(spec.gauge)
	}
}

func (r *registry) lookup(metricID string, dimCount int) (metricSpec, bool) {
	if spec, ok := r.preferred[metricID]; ok {
		return spec, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if spec, ok := r.dynamic[metricID]; ok {
		return spec, true
	}
	if dimCount < 0 {
		return metricSpec{}, false
	}
	labels := make([]string, dimCount)
	for i := range labels {
		labels[i] = fmt.Sprintf("dim_%d", i)
	}
	name := r.prefix + sanitizeMetricName(metricID)
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: name,
		Help: fmt.Sprintf("Auto-registered Dynatrace metric %s.", metricID),
	}, labels)
	if err := r.registrar.Register(gauge); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			if existing, ok := are.ExistingCollector.(*prometheus.GaugeVec); ok {
				gauge = existing
			} else {
				r.logger.Warn("metric name collision with non-gauge collector", "metric", metricID, "name", name)
				return metricSpec{}, false
			}
		} else {
			r.logger.Warn("dynamic metric registration failed", "metric", metricID, "name", name, "err", err)
			return metricSpec{}, false
		}
	}
	spec := metricSpec{gauge: gauge, labels: labels}
	r.dynamic[metricID] = spec
	r.logger.Info("registered dynamic metric", "dynatrace_id", metricID, "prometheus_name", name, "labels", labels)
	return spec, true
}

func sanitizeMetricName(id string) string {
	id = strings.TrimPrefix(id, "builtin:")
	id = strings.TrimPrefix(id, "ext:")
	var b strings.Builder
	b.Grow(len(id))
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_':
			b.WriteRune('_')
		default:
			if i == 0 {
				continue
			}
			b.WriteByte('_')
		}
	}
	out := b.String()
	out = strings.Trim(out, "_")
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	if out == "" {
		out = "unnamed"
	}
	return out
}

func pickLatestValue(values []*float64) (float64, bool) {
	for i := len(values) - 1; i >= 0; i-- {
		if values[i] == nil {
			continue
		}
		v := *values[i]
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v, true
		}
	}
	return 0, false
}

var (
	scrapeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "dynatrace_exporter_scrape_duration_seconds",
		Help:    "Time spent scraping the Dynatrace API per cycle.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	})
	scrapeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dynatrace_exporter_scrape_errors_total",
		Help: "Total scrape errors by reason (network, auth, rate_limit, server, client, parse).",
	}, []string{"reason"})
	lastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "dynatrace_exporter_last_successful_scrape_timestamp_seconds",
		Help: "Unix timestamp of the most recent successful scrape.",
	})
	apiRequestDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "dynatrace_exporter_api_request_duration_seconds",
		Help:    "Latency of individual Dynatrace API requests, including retries.",
		Buckets: prometheus.DefBuckets,
	})
	apiResponses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "dynatrace_exporter_api_responses_total",
		Help: "Count of Dynatrace API responses by HTTP status code.",
	}, []string{"code"})
	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dynatrace_exporter_build_info",
		Help: "Build info for the exporter. Value is always 1.",
	}, []string{"version"})
)

func registerSelfMetrics(registrar prometheus.Registerer) {
	registrar.MustRegister(scrapeDuration, scrapeErrors, lastSuccess, apiRequestDuration, apiResponses, buildInfo)
	buildInfo.WithLabelValues(version).Set(1)
}

type errReason string

const (
	reasonNetwork   errReason = "network"
	reasonAuth      errReason = "auth"
	reasonRateLimit errReason = "rate_limit"
	reasonServer    errReason = "server"
	reasonClient    errReason = "client"
	reasonParse     errReason = "parse"
)

type scrapeError struct {
	reason errReason
	err    error
	retry  bool
}

func (e *scrapeError) Error() string {
	return fmt.Sprintf("%s: %v", e.reason, e.err)
}

func callDynatrace(ctx context.Context, cfg *config, client *http.Client, logger *slog.Logger) (*metricResponse, *scrapeError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.APIURL, nil)
	if err != nil {
		return nil, &scrapeError{reason: reasonNetwork, err: err}
	}
	req.Header.Set("Authorization", "Api-Token "+cfg.APIToken)
	req.Header.Set("Accept", "application/json")

	q := req.URL.Query()
	q.Set("metricSelector", cfg.MetricSelector)
	q.Set("resolution", cfg.Resolution)
	q.Set("from", cfg.Lookback)
	q.Set("to", "now")
	if cfg.EntitySelector != "" {
		q.Set("entitySelector", cfg.EntitySelector)
	}
	req.URL.RawQuery = q.Encode()

	start := time.Now()
	resp, err := client.Do(req)
	apiRequestDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, &scrapeError{reason: reasonNetwork, err: err, retry: true}
	}
	defer resp.Body.Close()
	apiResponses.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		snippet := strings.TrimSpace(string(body))
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			return nil, &scrapeError{reason: reasonAuth, err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)}
		case resp.StatusCode == 429:
			return nil, &scrapeError{reason: reasonRateLimit, err: fmt.Errorf("HTTP 429: %s", snippet), retry: true}
		case resp.StatusCode >= 500:
			return nil, &scrapeError{reason: reasonServer, err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet), retry: true}
		default:
			return nil, &scrapeError{reason: reasonClient, err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)}
		}
	}

	var out metricResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, &scrapeError{reason: reasonParse, err: err}
	}
	logger.Debug("dynatrace response decoded", "metrics", len(out.Result))
	return &out, nil
}

func fetchWithRetry(ctx context.Context, cfg *config, client *http.Client, logger *slog.Logger) (*metricResponse, *scrapeError) {
	var lastErr *scrapeError
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, &scrapeError{reason: reasonNetwork, err: ctx.Err()}
		}
		resp, sErr := callDynatrace(ctx, cfg, client, logger)
		if sErr == nil {
			return resp, nil
		}
		lastErr = sErr
		if !sErr.retry || attempt == cfg.MaxRetries {
			return nil, sErr
		}
		backoff := time.Duration(1<<attempt)*time.Second + time.Duration(rand.Intn(500))*time.Millisecond
		logger.Warn("scrape attempt failed, retrying",
			"attempt", attempt+1,
			"reason", string(sErr.reason),
			"err", sErr.err,
			"backoff", backoff.String(),
		)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, &scrapeError{reason: reasonNetwork, err: ctx.Err()}
		}
	}
	return nil, lastErr
}

type scrapeStats struct {
	metrics int
	skipped int
	updated int
}

func updateMetrics(resp *metricResponse, reg *registry, logger *slog.Logger) scrapeStats {
	stats := scrapeStats{metrics: len(resp.Result)}
	for _, m := range resp.Result {
		var spec metricSpec
		var ok bool
		for _, dp := range m.Data {
			if len(dp.Values) == 0 {
				continue
			}
			if !ok {
				spec, ok = reg.lookup(m.MetricID, len(dp.Dimensions))
				if !ok {
					break
				}
			}
			if len(dp.Dimensions) < len(spec.labels) {
				stats.skipped++
				continue
			}
			value, hasValue := pickLatestValue(dp.Values)
			if !hasValue {
				stats.skipped++
				continue
			}
			labelValues := dp.Dimensions[:len(spec.labels)]
			spec.gauge.WithLabelValues(labelValues...).Set(value)
			stats.updated++
		}
	}
	logger.Debug("metric update done", "metrics", stats.metrics, "updated", stats.updated, "skipped", stats.skipped)
	return stats
}

func runScrape(ctx context.Context, cfg *config, client *http.Client, reg *registry, healthy *atomic.Bool, lastTS *atomic.Int64, logger *slog.Logger) {
	start := time.Now()
	defer func() {
		scrapeDuration.Observe(time.Since(start).Seconds())
	}()

	resp, sErr := fetchWithRetry(ctx, cfg, client, logger)
	if sErr != nil {
		scrapeErrors.WithLabelValues(string(sErr.reason)).Inc()
		healthy.Store(false)
		logger.Error("scrape failed", "reason", string(sErr.reason), "err", sErr.err)
		return
	}
	stats := updateMetrics(resp, reg, logger)
	now := time.Now().Unix()
	lastSuccess.Set(float64(now))
	lastTS.Store(now)
	healthy.Store(true)
	logger.Info("scrape ok",
		"metrics", stats.metrics,
		"updated", stats.updated,
		"skipped", stats.skipped,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func newHealthHandler(cfg *config, lastTS *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ts := lastTS.Load()
		if ts == 0 {
			http.Error(w, "no successful scrape yet", http.StatusServiceUnavailable)
			return
		}
		age := time.Since(time.Unix(ts, 0))
		if age > cfg.HealthyAfter {
			http.Error(w, fmt.Sprintf("stale: last scrape %s ago", age.Round(time.Second)), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "ok (last scrape %s ago)\n", age.Round(time.Second))
	}
}

func handleEarlyFlags(args []string, out io.Writer) (handled bool) {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "--version", "-v", "version":
		fmt.Fprintln(out, version)
		return true
	case "--help", "-h", "help":
		printHelp(out)
		return true
	}
	return false
}

func printHelp(w io.Writer) {
	fmt.Fprintf(w, `dynatrace-to-prometheus-exporter %s

Pulls host metrics from the Dynatrace Metrics v2 API and re-exposes them in
Prometheus format on :PORT/metrics.

Required environment variables:
  DYNATRACE_API_URL      https://YOUR_ENV.live.dynatrace.com/api/v2/metrics/query
  DYNATRACE_API_TOKEN    API token with metrics.read scope

Optional environment variables (with defaults):
  PORT                   8000
  SCRAPE_INTERVAL_SEC    60
  HTTP_TIMEOUT_SEC       10
  METRIC_SELECTOR        seven host builtins, see README
  ENTITY_SELECTOR        (empty)
  METRIC_PREFIX          dynatrace_
  RESOLUTION             1m
  LOOKBACK               now-10m
  MAX_RETRIES            3
  HEALTHY_AFTER_SEC      180

Endpoints:
  /metrics   Prometheus exposition
  /healthz   200 if last scrape was within HEALTHY_AFTER_SEC, 503 otherwise

Flags:
  --version, -v   Print version and exit
  --help, -h      Print this help and exit
`, version)
}

func main() {
	if handleEarlyFlags(os.Args, os.Stdout) {
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config invalid", "err", err)
		os.Exit(1)
	}

	registerSelfMetrics(prometheus.DefaultRegisterer)
	reg := newRegistry(cfg.MetricPrefix, prometheus.DefaultRegisterer, logger)
	reg.registerPreferred()

	logger.Info("dynatrace exporter starting",
		"version", version,
		"port", cfg.Port,
		"scrape_interval_sec", int(cfg.ScrapeInterval.Seconds()),
		"http_timeout_sec", int(cfg.HTTPTimeout.Seconds()),
		"metric_prefix", cfg.MetricPrefix,
		"resolution", cfg.Resolution,
		"lookback", cfg.Lookback,
		"max_retries", cfg.MaxRetries,
		"healthy_after_sec", int(cfg.HealthyAfter.Seconds()),
		"metric_selector", cfg.MetricSelector,
		"entity_selector_set", cfg.EntitySelector != "",
	)

	var (
		healthy atomic.Bool
		lastTS  atomic.Int64
	)

	client := &http.Client{Timeout: cfg.HTTPTimeout}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", newHealthHandler(cfg, &lastTS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "dynatrace-to-prometheus-exporter %s\n/metrics\n/healthz\n", version)
	})

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("http server starting", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop()
		}
	}()

	go func() {
		runScrape(ctx, cfg, client, reg, &healthy, &lastTS, logger)
		ticker := time.NewTicker(cfg.ScrapeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runScrape(ctx, cfg, client, reg, &healthy, &lastTS, logger)
			}
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received, draining")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown error", "err", err)
	}
	logger.Info("exporter stopped")
}
