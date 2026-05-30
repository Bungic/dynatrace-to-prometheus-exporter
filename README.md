# dynatrace-to-prometheus-exporter

A small Go service that pulls host metrics from the Dynatrace Metrics v2 API and re-exposes them in Prometheus format.

If you already run Prometheus everywhere and you have one stubborn Dynatrace-monitored cluster sitting outside that pipeline, this lets you scrape that cluster's host data without standing up a parallel Dynatrace integration in Grafana.

## What it scrapes

Seven host-level Dynatrace builtins by default:

- CPU usage and idle percentages
- Memory usage
- Disk available bytes and used percentage (per disk)
- Network bytes received and transmitted

Swap usage (`builtin:host.swap.used`) is supported through the preferred mapping but kept out of the default selector. Not every Dynatrace tenant exposes it, and the v2 API rejects an entire request when any one metric in the selector is unknown. If your tenant has it, add it explicitly via `METRIC_SELECTOR`.

Each one is republished as a Prometheus gauge with `host` (and `disk` where relevant) labels.

You can override the selector with `METRIC_SELECTOR` to pull a different set. Anything the Dynatrace v2 query API accepts is valid. If you ask for metrics this exporter has no mapping for, it logs them once at debug level and skips them rather than crashing.

## Configuration

| Variable | Default | What it does |
|---|---|---|
| `DYNATRACE_API_URL` | required | The metrics query endpoint |
| `DYNATRACE_API_TOKEN` | required | API token with `metrics.read` |
| `PORT` | `8000` | Where `/metrics` and `/healthz` are served |
| `SCRAPE_INTERVAL_SEC` | `60` | How often to poll Dynatrace |
| `HTTP_TIMEOUT_SEC` | `10` | Per-request timeout against the Dynatrace API |
| `METRIC_SELECTOR` | 7 host builtins | Dynatrace metric selector, comma-separated |
| `ENTITY_SELECTOR` | empty | Optional Dynatrace entity selector, e.g. `type(HOST),tag(env:prod)` |
| `METRIC_PREFIX` | `dynatrace_` | Prefix used when auto-registering unknown metrics |
| `RESOLUTION` | `1m` | Time resolution passed to the Dynatrace query API |
| `LOOKBACK` | `now-10m` | The `from` window passed to the Dynatrace query API |
| `MAX_RETRIES` | `3` | Backoff retries on transient errors (5xx, 429, network) |
| `HEALTHY_AFTER_SEC` | `180` | If no successful scrape inside this window, `/healthz` returns 503 |

The exporter refuses to start if `DYNATRACE_API_URL` or `DYNATRACE_API_TOKEN` is empty. That is deliberate. Silently scraping with no token isn't helpful to anyone.

Run `./dynatrace-exporter --help` for the same table from the binary, or `--version` for the build tag.

## Choosing what to scrape

The default `METRIC_SELECTOR` covers seven common host builtins. To scrape anything else, override it with whatever the Dynatrace v2 query API accepts:

```bash
export METRIC_SELECTOR="builtin:service.response.time,builtin:service.errors.total.rate,builtin:apps.web.actionCount"
```

You do not need to teach the exporter about new metric IDs. Metrics with a built-in mapping (the seven defaults plus `builtin:host.swap.used`) keep their friendly names and labels. Anything else is auto-registered on the first scrape:

- The metric ID is sanitized (drops the `builtin:` or `ext:` prefix, replaces `:` and `.` with `_`)
- The exporter prepends `METRIC_PREFIX`, so `builtin:service.response.time` becomes `dynatrace_service_response_time`
- Dimensions are exposed as labels named `dim_0`, `dim_1`, ... in the order Dynatrace returns them

A log line at info level records every dynamic registration:

```json
{"msg":"registered dynamic metric","dynatrace_id":"builtin:service.response.time","prometheus_name":"dynatrace_service_response_time","labels":["dim_0","dim_1"]}
```

If you need pretty label names like `service` or `endpoint` for dynamic metrics, relabel downstream with `metric_relabel_configs` in Prometheus. Adding them to the exporter would require a metadata fetch per metric on every start, which is more cost than the trade is worth.

## Endpoints

- `/metrics`: Prometheus exposition. Your Dynatrace metrics plus exporter self-metrics (prefixed `dynatrace_exporter_*`).
- `/healthz`: returns 200 if the last scrape succeeded inside `HEALTHY_AFTER_SEC`, 503 otherwise.
- `/`: short summary listing the two paths above.

## Self-metrics

```
dynatrace_exporter_scrape_duration_seconds                  (histogram)
dynatrace_exporter_scrape_errors_total{reason="..."}        (counter)
dynatrace_exporter_last_successful_scrape_timestamp_seconds (gauge)
dynatrace_exporter_api_request_duration_seconds             (histogram, includes retries)
dynatrace_exporter_api_responses_total{code="..."}          (counter)
dynatrace_exporter_build_info{version="..."}                (gauge, always 1)
```

The error counter is the one to alert on. `reason="auth"` means the token is wrong. `reason="rate_limit"` showing up steadily means you should ask Dynatrace for a higher quota or slow down `SCRAPE_INTERVAL_SEC`.

## Retry behavior

Transient errors get retried with exponential backoff: 1s, 2s, 4s, 8s (plus up to 500ms jitter). What counts as transient:

- HTTP 5xx
- HTTP 429
- Network errors (DNS, connection refused, TLS handshake)

401 and 403 are not retried. Those mean the token is bad, and hammering Dynatrace harder won't change that.

## Logging

Structured JSON via `log/slog`. Each line includes the scrape result, error reason, duration, and retry attempt where applicable. Ship to Loki, OpenSearch, whatever. Nothing custom to parse.

---

## Build from source

```bash
git clone https://github.com/Bungic/dynatrace-to-prometheus-exporter.git
cd dynatrace-to-prometheus-exporter
go build -trimpath -ldflags="-s -w" -o dynatrace-exporter .
```

Run it directly to smoke-test:

```bash
export DYNATRACE_API_URL="https://YOUR_ENV.live.dynatrace.com/api/v2/metrics/query"
export DYNATRACE_API_TOKEN="dt0c01.YOUR_TOKEN"
./dynatrace-exporter
```

Then in another shell: `curl localhost:8000/metrics`.

---

## Deploy as a systemd service

For Linux servers without Docker. Builds once, runs forever, restarts on failure.

Everything you need is under [`deploy/systemd/`](deploy/systemd):

```
deploy/systemd/
├── dynatrace-exporter.service   the unit, hardened
├── dynatrace-exporter.env       env file template
├── install-systemd.sh           one-shot installer
└── uninstall-systemd.sh         clean removal
```

### Quick install

```bash
sudo ./deploy/systemd/install-systemd.sh
sudo $EDITOR /etc/dynatrace-exporter/.env       # fill in URL + token
sudo systemctl enable --now dynatrace-exporter
journalctl -u dynatrace-exporter -f
```

The script builds the binary from source (or uses `BINARY=/path/to/binary` if you already have one), creates a `dynatrace-exporter` system user, lays down the env file at `/etc/dynatrace-exporter/.env` (mode `0640`, owned `root:dynatrace-exporter`), installs the unit, and reloads systemd. It's idempotent: re-running it upgrades the binary and unit without touching your env file.

Pass `VERSION=1.0.0` to stamp the build with a version tag visible through `--version` and the `dynatrace_exporter_build_info` metric.

### What the unit does

The shipped `dynatrace-exporter.service` runs the process as an unprivileged user under a tight sandbox: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, all capabilities dropped, namespace creation blocked, address families limited to AF_INET/AF_INET6, native syscalls only. Inspect it before applying.

Logs go to the journal in structured JSON. Point your log shipper at `_SYSTEMD_UNIT=dynatrace-exporter.service`.

### Removal

```bash
sudo ./deploy/systemd/uninstall-systemd.sh           # keep env + user
sudo PURGE=1 ./deploy/systemd/uninstall-systemd.sh   # nuke everything
```

---

## Deploy with Docker

For when you have a container runtime but no Kubernetes.

Build the image:

```bash
docker build -t dynatrace-exporter:1.0.0 .
```

The image is multi-stage and based on `gcr.io/distroless/static-debian12:nonroot`. Final size is around 15 MB. Runs as a nonroot user, no shell, no package manager.

Run it:

```bash
docker run -d \
  --name dynatrace-exporter \
  --restart unless-stopped \
  -p 8000:8000 \
  -e DYNATRACE_API_URL="https://YOUR_ENV.live.dynatrace.com/api/v2/metrics/query" \
  -e DYNATRACE_API_TOKEN="dt0c01.YOUR_TOKEN" \
  dynatrace-exporter:1.0.0
```

For credentials in a file rather than the shell history, copy `deploy/systemd/dynatrace-exporter.env` somewhere, edit it, and use `--env-file path/to/env`.

If you need to pull the image from another host, push it to your own registry:

```bash
docker tag dynatrace-exporter:1.0.0 your-registry.example.com/dynatrace-exporter:1.0.0
docker push your-registry.example.com/dynatrace-exporter:1.0.0
```

This repository does not publish a prebuilt image. Build your own and host it wherever makes sense for you.

---

## Deploy on Kubernetes

The manifest lives at [`deploy/kubernetes/manifest.yaml`](deploy/kubernetes/manifest.yaml) and ships a Secret, a Deployment, and a ClusterIP Service.

First build and push the image to your registry (see the Docker section). Then edit `deploy/kubernetes/manifest.yaml`:

- Replace `<your-registry>/dynatrace-exporter:1.0.0` with the image you pushed.
- Fill in `api-url` and `api-token` in the Secret, or replace the Secret with one you manage via Sealed Secrets, ExternalSecrets, Vault, etc.

Apply:

```bash
kubectl apply -f deploy/kubernetes/manifest.yaml
kubectl rollout status deployment/dynatrace-exporter
kubectl port-forward svc/dynatrace-exporter 8000:8000
curl localhost:8000/metrics
```

To scrape from Prometheus: with prometheus-operator, a `PodMonitor` or `ServiceMonitor` selecting `app=dynatrace-exporter` on the `metrics` port at path `/metrics` is enough. For plain Prometheus, add a `kubernetes_sd_configs` job that selects the same label.

The Deployment includes resource requests and limits (10m CPU / 32Mi memory request, 100m / 64Mi limit), a readonly root filesystem, nonroot user, and dropped capabilities. Adjust to fit your cluster's standards.

---

## Gotchas

The Dynatrace v2 API charges per data point on some plans. Polling seven metrics every 60 seconds across a 1000-host fleet adds up. Check the bill before pointing this at production.

The exporter queries `from=now-10m` at 1-minute resolution and takes the latest value per series. That is fine for gauges and percentages. If you need rate semantics, do the rate calculation downstream in PromQL.

There is no auth on `/metrics`. Run it behind a network policy or a firewall rule that limits scraping to your Prometheus instance.

## What's not in here

- Service-level metrics (response time, throughput). The metric set is host-focused.
- Native percentile metric ingestion. Dynatrace percentiles come back as separate gauges per percentile, which works but isn't pretty.
- Multi-tenant fan-out. The process talks to exactly one Dynatrace tenant (one URL + one token). If you need to scrape several tenants from the same Prometheus, run one exporter instance per tenant, on different ports, and add one scrape target per instance in your Prometheus config. The exporter has no notion of "list of tenants" inside one process, and that's deliberate: each instance fails independently, gets its own resource limits, and can be restarted without touching the others.

If you want any of those, fork it. The code is one file.

## License

MIT. See [LICENSE](LICENSE).
