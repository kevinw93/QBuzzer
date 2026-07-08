# QBuzzer

Live quiz bowl buzzer: a moderator reads over voice chat, players buzz in the
browser, and the server decides who was first.

Requires Go 1.27+, Node 22 (`mise install` reads `mise.toml`), and
golangci-lint v2 for `make lint`.

```sh
make dev    # Go server + Vite dev server at http://localhost:5173
make test   # go test -race ./...
make lint   # golangci-lint + eslint
make gen    # regenerate web/src/protocol.ts from internal/protocol
make build  # frontend build, then a single binary at bin/qbuzzer
```

The server listens on `127.0.0.1:8080`; set `QBUZZER_ADDR` to override.

## Layout

| Path | Contents |
|---|---|
| `cmd/server` | entrypoint, config, graceful shutdown |
| `internal/protocol` | wire types; source for the generated TypeScript |
| `internal/room` | hub and per-room event loop |
| `internal/httpapi` | HTTP routes and WebSocket pumps |
| `internal/clock` | clock offset and latency estimation |
| `internal/game` | tossup/bonus state machine and scoring |
| `internal/store` | PostgreSQL persistence |
| `web` | React + TypeScript frontend, embedded into the binary |
