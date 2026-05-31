# goboxd

goboxd is an HTTP sandbox runner that accepts source code, compiles or interprets it inside a resource-limited subprocess, and returns stdout, stderr, exit code, and runtime metrics.

## Framework

The server uses `net/http` from the Go standard library. It covers everything the spec requires without pulling in a third-party dependency, and the 1.22 method-based routing (`GET /healthz`, `POST /run`) removes the only reason to reach for a router.

## Running

Start the service with Docker Compose:

```
docker compose up
```

All other operations go through the Makefile:

| Target           | What it does                          |
|------------------|---------------------------------------|
| `make build`     | Compile the binary                    |
| `make run`       | Build and start the server            |
| `make test`      | Run unit tests                        |
| `make integration` | Run integration tests              |
| `make lint`      | Run the linter                        |

## Docs

Architecture decisions and development notes are in `docs/ai/`.
