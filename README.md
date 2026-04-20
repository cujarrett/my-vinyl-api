# my-vinyl-api

Go proxy that exposes a Discogs vinyl collection as a clean JSON API.

## Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/collection` | Paginated fetch of all releases, trimmed to `[{id, artist, title, year, label, cover_url}]` |
| `GET` | `/collection?username=foo` | Same, for a different Discogs user (defaults to `cujarrett`) |
| `GET` | `/health` | Returns `{"status":"ok"}` |

## Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `DISCOGS_TOKEN` | yes | — | Discogs API token from [discogs.com/settings/developers](https://www.discogs.com/settings/developers) |
| `PORT` | no | `8080` | Port the server listens on |

## Run locally

```bash
export DISCOGS_TOKEN=your_token_here
go run .
curl http://localhost:8080/collection
```
