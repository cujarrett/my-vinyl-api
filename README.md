# my-vinyl-api

Go proxy that exposes a Discogs vinyl collection as a clean JSON API.

## Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/collection` | Returns one page of the collection as a paginated envelope |
| `GET` | `/health` | Returns `{"status":"ok","version":"..."}` |

### `GET /collection` query params

| Param | Default | Description |
|---|---|---|
| `username` | `cujarrett` | Discogs username |
| `page` | `1` | Page number (>= 1, must not exceed total pages) |

Response shape:

```json
{
  "page": 1,
  "pages": 12,
  "items": 573,
  "releases": [
    { "id": 1, "artist": "...", "title": "...", "year": 2001, "label": "...", "cover_url": "..." }
  ]
}
```

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
