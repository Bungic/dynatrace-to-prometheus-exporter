# Changelog

## 1.0.0 - 2026-05-30

Initial release.

Polls the Dynatrace Metrics v2 API for seven host builtins by default: CPU usage and idle, memory, disk available and used percent, and network bytes received and transmitted. Republishes them as Prometheus gauges with `host` (and `disk` where relevant) labels. Swap usage (`builtin:host.swap.used`) has a built-in mapping too, but it's left out of the default selector because not every tenant has it.

Any metric returned by `METRIC_SELECTOR` that the exporter does not recognize is auto-registered on the first scrape. The Dynatrace metric ID is sanitized (drops the `builtin:` / `ext:` prefix, replaces `:` and `.` with `_`), prefixed with `METRIC_PREFIX` (default `dynatrace_`), and exposed with `dim_0`, `dim_1`, ... labels in dimension order. The value picker walks each series from the latest bucket backwards and returns the first non-null, non-NaN, non-Inf reading, which matters because Dynatrace fills the most recent buckets with `null` until aggregation completes.

Self-metrics under `dynatrace_exporter_*`:

- `scrape_duration_seconds` (histogram)
- `scrape_errors_total{reason}` (counter)
- `last_successful_scrape_timestamp_seconds` (gauge)
- `api_request_duration_seconds` (histogram, includes retries)
- `api_responses_total{code}` (counter)
- `build_info{version}` (gauge)

Reliability:

- Exponential backoff with jitter for 5xx, 429, and network errors. 401 and 403 fail fast.
- Errors are categorized into one of `auth`, `rate_limit`, `server`, `client`, `parse`, `network`.
- Structured JSON logging via `log/slog`. Startup emits the full effective configuration in one line.
- `/healthz` returns 503 if no scrape has succeeded inside `HEALTHY_AFTER_SEC` (default 180s).
- Graceful shutdown on SIGINT and SIGTERM via `signal.NotifyContext`, with a 10-second drain.

Configuration is all environment variables: `PORT`, `SCRAPE_INTERVAL_SEC`, `HTTP_TIMEOUT_SEC`, `METRIC_SELECTOR`, `ENTITY_SELECTOR`, `METRIC_PREFIX`, `RESOLUTION`, `LOOKBACK`, `MAX_RETRIES`, `HEALTHY_AFTER_SEC`. The exporter refuses to start if `DYNATRACE_API_URL` or `DYNATRACE_API_TOKEN` is empty. CLI flags: `--version`, `--help`.

Packaging:

- Multi-stage Dockerfile based on `gcr.io/distroless/static-debian12:nonroot`. Image is around 15 MB.
- Systemd unit and env file template under `deploy/systemd/`, with `install-systemd.sh` and `uninstall-systemd.sh` for one-shot deploys. The unit ships with the usual hardening directives (`NoNewPrivileges`, `ProtectSystem=strict`, dropped capabilities, restricted address families).
- Kubernetes manifest at `deploy/kubernetes/manifest.yaml` (Secret + Deployment + Service). Resource limits, readonly root filesystem, nonroot user, capabilities dropped.
- GitHub Actions CI runs `go vet`, `go build`, and `go test -race` on every push and PR.
- Version is injected at build time via `-ldflags="-X main.version=..."` and surfaced through `--version` and the `dynatrace_exporter_build_info` metric.
