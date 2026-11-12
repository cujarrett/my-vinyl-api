# my-vinyl-api

## Rules

- **Never run `git commit`, `git push`, or any git command that writes to or modifies repository history or remotes.** If a task requires committing or pushing, stop and tell the user to run the git command manually.
- **Never add a `Co-Authored-By` trailer or a "Generated with Claude Code" line** to commit messages or PR descriptions, including in suggested commit messages. Commits are authored by the user alone.
- Standard library first. The Prometheus client and go-redis are the only dependencies.
- Put state on the `app` struct, with no global variables.
- Read config once in `main()`. Handlers never call `os.Getenv`.
- Keep all application code in `main.go` and all tests in `main_test.go` unless the file grows large enough to warrant splitting.

### Pre-commit safety check

Before telling the user to commit, always run `/security-review`. It reviews the pending changes on the current branch for security issues. Once it confirms the changes are safe, offer the user a suggested commit message - do not run `git commit` yourself.

## Project Overview

A Go REST API that proxies the Discogs API to serve a vinyl collection. It runs on the homelab cluster (ARM64) and is cluster-internal, at `my-vinyl-api.local.lab`. The SPA at `https://myvinyl.mattjarrett.dev` reaches it through the platform's nginx, which proxies `/api/` to it.

## Tech Stack

- **Language**: Go, standard library first
- **Container**: Multi-stage Dockerfile, `linux/arm64`, non-root user
- **CI/CD**: GitHub Actions → GHCR (`ghcr.io/cujarrett/my-vinyl-api`)
- **Deployment**: a homelab `Api`, defined at `my-vinyl/my-vinyl-api.yaml` in `homelab-workspaces`

## Project Structure

```
main.go          # All application code, in a single file
main_test.go     # All tests, httptest only, no real network calls
go.mod           # Module: github.com/cujarrett/my-vinyl-api
Dockerfile       # Multi-stage ARM64 build
.github/
  workflows/
    ci.yml       # test, build-and-push, deploy
```

## Architecture

- `app` struct holds every dependency: the Discogs settings, the Prometheus metrics and the optional Redis client
- Config is read once at startup in `main()`, never per-request
- Handlers are methods on `*app` for testability without globals
- The cache is Redis, found through the `cache` service binding. With no binding, caching is off
- CORS is locked to `https://myvinyl.mattjarrett.dev`, never a wildcard

## Endpoints

[README → Endpoints](./README.md#endpoints) has `/collection` and `/healthz`. `/metrics` is served on `METRICS_PORT`, apart from the main port.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `DISCOGS_TOKEN` | none | Discogs API token, for local dev |
| `DISCOGS_TOKEN_FILE` | `/secrets/discogs-credentials/DISCOGS_TOKEN` | Path to the token file the platform mounts |
| `DISCOGS_CONSUMER_KEY`, `DISCOGS_CONSUMER_SECRET` | none | Used together in place of a token |
| `PORT` | `8080` | Port to listen on |
| `METRICS_PORT` | `9090` | Port for `/metrics` |
| `SERVICE_BINDING_ROOT` | `/bindings` | Where the platform mounts service bindings |
| `CACHE_TTL` | `5m` | How long a cached Discogs response lives |

## Coding Conventions

- Fail fast: use `log.Fatal` at startup for missing required config
- `defer func() { _ = resp.Body.Close() }()` is the explicit discard that `errcheck` wants
- All tests use `httptest.NewServer` and `httptest.NewRecorder`, with no real network calls and no env var dependencies

## Local Development

`just ci` runs lint, test and build. `DISCOGS_TOKEN=your_token just run` starts it locally. `just --list` shows every recipe.

## CI/CD

- **`test` job**: runs on all pushes and PRs. `go test ./...` then `go vet ./...`.
- **`build-and-push` job**: runs on `main` after `test` passes. Builds the ARM64 image and pushes it to GHCR with `:main` and `:sha-<sha>` tags.
- **`deploy` job**: runs after `build-and-push`. Updates the image tag in `homelab-workspaces`.

## Version

Set at build time via `-ldflags="-X main.version=x.y.z"` in the Dockerfile. Defaults to `"dev"` when running locally with `go run`.

## Philosophy: Grug-Brained Development

> "Complexity very, very bad." - [grugbrain.dev](https://grugbrain.dev/)

- **Say no.** The best weapon against complexity is the word "no". No new feature, no new abstraction, until it earns its place.
- **Cheapest rung that works.** Before writing code go down the ladder and stop at the first rung that solves it - skip the feature, reuse code already here, standard library, native platform feature, a dependency already installed, one line, then build the minimum.
- **No abstraction until a pattern repeats three times.** Let cut points emerge naturally from the code; don't invent them up front.
- **80/20 solutions.** Ship 80% of the value with 20% of the code. Ugly but working beats elegant but over-engineered.
- **Chesterton's Fence.** Understand why code exists before removing it. If you don't see the use, go away and think.
- **Boring, obvious code wins.** Intermediate variables with good names beat clever one-liners. Easier to debug.
- **DRY is not a law.** A little copy-paste beats a complex abstraction built for two cases.
- **No FOLD** (Fear Of Looking Dumb). If something is too complex, say so. That's a signal to simplify, not a personal failing.
