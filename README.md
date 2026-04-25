# file-service

A lightweight file proxy service built with Go + Gin. Sits between your app and any S3-compatible storage (currently t3.dev / Tigris) and exposes a clean two-endpoint API for uploading and retrieving files via presigned URLs.

## Endpoints

### `GET /health`
Returns service status.

### `GET /get-upload-uri`
Generates a server-side UUID key and returns a presigned S3 PUT URL valid for **15 minutes**.

| Query param | Required | Description |
|---|---|---|
| `prefix` | No | Exactly 5 characters, prepended to the key |
| `suffix` | No | Exactly 5 characters, appended to the key |

**Response:**
```json
{
  "key": "imgs1-550e8400-e29b-41d4-a716-446655440000-thumb1",
  "uri": "https://...",
  "expires_in": "15m"
}
```

### `GET /get-file-uri`
Returns a presigned S3 GET URL for an existing file, valid for **1 hour**.

| Query param | Required | Description |
|---|---|---|
| `key` | Yes | The key returned from `/get-upload-uri` |

**Response:**
```json
{
  "uri": "https://...",
  "expires_in": "1h"
}
```

## Running locally

```bash
cp .env.example .env   # fill in your credentials
go run main.go
```

Default port is `8080`. Override with `PORT=9000 go run main.go`.

## Environment variables

| Variable | Description |
|---|---|
| `S3_ENDPOINT` | S3-compatible endpoint URL |
| `S3_REGION` | Region (use `auto` for Tigris/t3.dev) |
| `S3_BUCKET` | Bucket name |
| `S3_ACCESS_KEY_ID` | Access key ID |
| `S3_SECRET_ACCESS_KEY` | Secret access key |
| `PORT` | Server port (default: `8080`) |

## Logs

Every request logs memory footprint and running counters:

```
[STARTUP]   file-service running on :8080 | bucket=my-bucket endpoint=https://t3.storageapi.dev
[UPLOAD-URI] key=imgs1-<uuid> total_upload_uris=42
[FILE-URI]   key=imgs1-<uuid> total_file_uris=17
[MEM/REQ]   path=/get-upload-uri alloc=1.07MB goroutines=4
[MEMORY]    alloc=1.21MB sys=8.50MB gc_cycles=2 goroutines=4   ← every 60s
```
