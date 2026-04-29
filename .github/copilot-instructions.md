# Copilot Instructions — my-vinyl-api

## Copilot Rules

- **Never run `git commit`, `git push`, or any git command that writes to or modifies repository history or remotes.** If a task requires committing or pushing, stop and tell the user to run the git command manually.
- Do not add external dependencies — stdlib only.
- Do not use global variables — put state on the `app` struct.
- Do not call `os.Getenv` inside handlers — read config once in `main()`.
- Keep all application code in `main.go` and all tests in `main_test.go` unless the file grows large enough to warrant splitting.

## Project Overview

A Go REST API that proxies the Discogs API to serve a personal vinyl collection. Deployed on a homelab Kubernetes cluster (ARM64) via Cloudflare Tunnel at `https://my-vinyl-api.mattjarrett.dev`. The SPA that consumes this API is served from `https://myvinyl.mattjarrett.dev`.

## Tech Stack

- **Language**: Go 1.26, stdlib only — no external dependencies
- **Container**: Multi-stage Dockerfile, `linux/arm64`, non-root user
- **CI/CD**: GitHub Actions → GHCR (`ghcr.io/cujarrett/my-vinyl-api`)
- **Deployment**: Kubernetes via homelab XApi Crossplane XR

## Project Structure

```
main.go          # All application code — single file
main_test.go     # All tests — httptest only, no real network calls
go.mod           # Module: github.com/cujarrett/my-vinyl-api
Dockerfile       # Multi-stage ARM64 build
.github/
  workflows/
    ci.yml       # Test + lint on PRs, build+push to GHCR on main
```

## Architecture

- `app` struct holds all dependencies (`discogsBase`, `httpClient`, `token`, `defaultUsername`)
- Config is read once at startup in `main()`, never per-request
- Handlers are methods on `*app` for testability without globals
- Pagination follows Discogs `pagination.urls.next` in a loop until empty
- CORS locked to `https://myvinyl.mattjarrett.dev` — not a wildcard

## Endpoints

| Method | Path          | Description                                  |
|--------|---------------|----------------------------------------------|
| GET    | `/health`     | Liveness probe — returns `{"status":"ok","version":"x.y.z"}` |
| GET    | `/collection` | Full paginated Discogs collection as flat JSON array |

Query params: `?username=` overrides the default username (`cujarrett`).

## Environment Variables

| Variable        | Required | Default    | Description            |
|-----------------|----------|------------|------------------------|
| `DISCOGS_TOKEN` | Yes      | —          | Discogs API token      |
| `PORT`          | No       | `8080`     | Port to listen on      |

## Coding Conventions

- Stdlib only — do not add external dependencies to `go.mod`
- No package-level globals — all state lives on the `app` struct
- Read config once at startup, not per-request
- Fail fast: use `log.Fatal` at startup for missing required config
- `defer func() { _ = resp.Body.Close() }()` — explicit discard for `errcheck`
- All tests use `httptest.NewServer` / `httptest.NewRecorder` — no real network calls, no env var dependencies

## Local Development

```bash
# Run tests
go test ./...

# Lint
go vet ./...

# Build
go build -o my-vinyl-api .

# Run (requires DISCOGS_TOKEN)
DISCOGS_TOKEN=your_token ./my-vinyl-api
```

## CI/CD

- **`test` job**: runs on all pushes and PRs — `go test ./...` then `go vet ./...`
- **`build-and-push` job**: runs on `main` only after `test` passes — builds ARM64 Docker image and pushes to GHCR with `:main` and `:sha-<sha>` tags

## Version

Set at build time via `-ldflags="-X main.version=x.y.z"` in the Dockerfile. Defaults to `"dev"` when running locally with `go run`.

## Philosophy: Grug-Brained Development

> "Complexity very, very bad." — [grugbrain.dev](https://grugbrain.dev/)

- **Say no.** The best weapon against complexity is the word "no". No new feature, no new abstraction, until it earns its place.
- **No abstraction until a pattern repeats three times.** Let cut points emerge naturally from the code; don't invent them up front.
- **80/20 solutions.** Ship 80% of the value with 20% of the code. Ugly but working beats elegant but over-engineered.
- **Chesterton's Fence.** Understand why code exists before removing it. If you don't see the use, go away and think.
- **Boring, obvious code wins.** Intermediate variables with good names beat clever one-liners. Easier to debug.
- **DRY is not a law.** A little copy-paste beats a complex abstraction built for two cases.
- **No FOLD** (Fear Of Looking Dumb). If something is too complex, say so. That's a signal to simplify, not a personal failing.
