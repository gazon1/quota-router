# quota-router

Minimal Go quota controller bootstrap.

## Build

```sh
go build ./cmd/quota-controller
```

## Run

```sh
go run ./cmd/quota-controller
```

The server listens on `:8080` by default and exposes `GET /health`, which returns `{"status":"ok"}`.

## Configuration

Configuration is read from environment variables. Invalid values fail fast with actionable error messages; when multiple values are invalid, errors are reported together.

Empty duration variables use defaults. `QUOTA_ROUTER_ADDR` must not be empty when set.

| Variable | Default | Description |
|---|---:|---|
| `QUOTA_ROUTER_ADDR` | `:8080` | HTTP listen address |
| `QUOTA_ROUTER_READ_HEADER_TIMEOUT` | `5s` | HTTP read header timeout (Go duration) |
| `QUOTA_ROUTER_READ_TIMEOUT` | `5s` | HTTP read timeout (Go duration) |
| `QUOTA_ROUTER_WRITE_TIMEOUT` | `10s` | HTTP write timeout (Go duration) |
| `QUOTA_ROUTER_IDLE_TIMEOUT` | `60s` | HTTP idle timeout (Go duration) |
| `QUOTA_ROUTER_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown timeout (Go duration) |

## Development

```sh
gofmt -l .        # should print nothing
go vet ./...
go test ./...
```
