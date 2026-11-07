# Self-Hosted YT Music Server (Go)

Backend for the self-hosted YouTube Music alternative: auth, SQLite state, yt-dlp -> ffmpeg -> Opus
streaming, smart cache with eviction, REST API.

## Run

Docker:   `docker compose up -d --build`
Bare metal (needs `ffmpeg` and `yt-dlp` in PATH, Go 1.22+):
`go mod tidy && go build -o ytm-server . && REGISTRATION_KEY=secret ./ytm-server`

## Getting your YouTube Music cookie
1. Open https://music.youtube.com while logged in, open DevTools -> Network.
2. Click any request to `music.youtube.com/youtubei/v1/...`, copy the full `Cookie` request header.
3. It must contain `SAPISID` / `__Secure-3PAPISID`. Cookies expire; update with `PUT /api/v1/auth/cookie`.

## Quick test
```
curl -X POST localhost:8080/api/v1/auth/register -H 'X-Registration-Key: change-me' \
  -d '{"username":"me","cookie":"<paste Cookie header>"}'          # -> {"token":"ytm_..."}
T=ytm_...
curl -H "Authorization: Bearer $T" localhost:8080/api/v1/home
curl -H "Authorization: Bearer $T" "localhost:8080/api/v1/search?q=daft+punk&filter=songs"
curl -H "Authorization: Bearer $T" localhost:8080/api/v1/stream/dQw4w9WgXcQ -o song.opus
```

## API (all except register/healthz need `Authorization: Bearer <token>`)
| Method | Path | Notes |
|---|---|---|
| POST | /api/v1/auth/register | `{username, cookie}`; header `X-Registration-Key` if configured |
| PUT | /api/v1/auth/cookie | `{cookie}` refresh expired cookie |
| GET | /api/v1/home | FYP / mixes (sections of items) |
| GET | /api/v1/search?q=&filter=songs | |
| GET | /api/v1/library/playlists | user's YT Music library playlists |
| GET | /api/v1/ytplaylist/{id} | tracks of a YT playlist/album |
| GET | /api/v1/stream/{videoId} | chunked Opus (Ogg); Range-capable once cached |
| GET | /api/v1/download/{videoId} | full file with Content-Length for offline |
| GET/POST | /api/v1/playlists | local playlists |
| GET/DELETE | /api/v1/playlists/{id} | |
| POST | /api/v1/playlists/{id}/tracks | `{videoId,title,artist}` |
| DELETE | /api/v1/playlists/{id}/tracks/{videoId} | |
| GET/POST | /api/v1/history | |
| GET/PUT | /api/v1/preferences | string map |

## Config (env)
PORT=8080, DATA_DIR, CACHE_DIR, CACHE_THRESHOLD=3, MAX_CACHE_GB=20, MAX_CACHE_TRACKS=5000,
EVICT_INTERVAL=10m, LOG_LEVEL=debug|info|warn|error, REGISTRATION_KEY, OPUS_BITRATE=96k,
MAX_TRANSCODES=4, YTDLP_PATH, FFMPEG_PATH, YTDLP_EXTRA_ARGS.

## Behaviour notes
- Every uncached stream is also teed to a temp file; if it completes, the per-track full-stream
  counter increments and, at the threshold, the file is promoted to `/cache/audio`. Counting is
  global per track (across users).
- Eviction score = play_count / (1 + days since last access); lowest evicted until under 90% of caps.
- Logs: console (text) + `/cache/logs/server.log` (JSON, 10 MB x 5 rotation).
- The YouTube Music client is unofficial (cookie auth, like ytmusicapi). Response parsing is
  generic and may need tweaks if YouTube changes its JSON. Keep yt-dlp updated.
