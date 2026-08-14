# Deploying to Fly.io

One machine in `ord` (Chicago), Fly Managed Postgres on Fly's private network,
TLS terminated by Fly's proxy.

```
browser ──https/wss:443──▶ Fly proxy (TLS) ──▶ qbuzzer :8080 ──▶ Managed Postgres (private network)
                                                  :9091 /metrics ◀── Fly metrics scraper (private)
```

Rooms live in one process's memory, so never run more than one machine:
always `--ha=false`.

## One-time setup

Requires `flyctl` and a Fly account. These create billable resources and
make the app public.

```sh
fly auth login
fly apps create qbuzzer-kevinw93          # or edit `app` in fly.toml to a free name

# Private network only; never give it a public endpoint.
fly mpg create --name qbuzzer-db --region ord
fly mpg attach qbuzzer-db -a qbuzzer-kevinw93   # sets the DATABASE_URL secret

fly deploy --ha=false
fly scale count 1 -a qbuzzer-kevinw93
```

The `fly mpg` commands are unverified; check `fly mpg --help`.

CI deploys need a deploy token as the `FLY_API_TOKEN` secret of the
repository's `production` environment, and that environment needs a required
reviewer:

```sh
fly tokens create deploy -a qbuzzer-kevinw93
```

## Verify after the first deploy

```sh
curl -s https://qbuzzer-kevinw93.fly.dev/healthz      # "ok"
fly logs -a qbuzzer-kevinw93                           # JSON logs: "recovered games", "listening"
fly status -a qbuzzer-kevinw93                         # exactly one machine, in ord
fly mpg list                                           # the database has no public address
```

Then play one tossup from a phone on cellular data. Metrics are in Fly's
managed Grafana.

## Deploys

Every deploy restarts the only machine: games are restored and clients
reconnect within seconds, but play pauses. **Never deploy during a
tournament.**

Rollback: `fly releases -a qbuzzer-kevinw93`, then
`fly deploy --image <previous image> --ha=false`.

## Configuration

| Variable | Production value | Notes |
|---|---|---|
| `QBUZZER_ADDR` | `0.0.0.0:8080` | Inside the VM; only Fly's proxy reaches it |
| `DATABASE_URL` | secret | Set by `fly mpg attach` |
| `QBUZZER_CLIENT_IP_HEADER` | `Fly-Client-IP` | Trusted only because Fly's proxy overwrites it |
| `QBUZZER_METRICS_ADDR` | `[::]:9091` | Private network; not an http_service |
| `QBUZZER_LOG_FORMAT` | `json` | |
| `QBUZZER_RATE_LIMITS` | (unset: on) | `off` only for local load tests |
| `QBUZZER_DEBUG_ADDR` | (unset) | Loopback-only pprof; for local profiling |

Rate limits per client IP: 10 room creations/minute, 120 WebSocket
joins/minute (burst 60). A whole school network shares one IP; if
`qbuzzer_rate_limited_total` climbs during real use, raise the join limit.
