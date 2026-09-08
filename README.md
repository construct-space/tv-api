# tv-api — Construct TV

`construct/tv` — the backend for the **Construct TV** ambient dashboard. A small Go
service (stdlib `net/http`) that serves the embedded Construct-design-system frontend,
proxies weather, and turns a spoken query into a typed **screen-spec** by calling the
Construct **inference** API. Same shape as the other `api/*` services.

## Run
```
go run .          # http://localhost:8087   (PORT to override)
```

## Endpoints
| Method | Path | Purpose |
|---|---|---|
| GET  | `/api/health` | status, model, token usage |
| GET  | `/api/weather?city=Ferizaj` (or `?lat=&lon=`) | Open-Meteo current + 6-day (no key) |
| POST | `/api/ask` `{query}` | `{spec, meta}` — screen-spec via inference, local fallback |
| GET  | `/` | the embedded `web/` SPA (Construct DS: Rubik, coral `#FF2D55`, light+dark) |

## Config (env)
| Var | Default | |
|---|---|---|
| `PORT` | `8087` | listen port |
| `INFERENCE_URL` | `https://llm.lisaos.dev` | Construct inference base |
| `INTERNAL_SHARED_SECRET` | — | sent as `X-Internal-Secret` to inference (required in prod) |
| `TV_MODEL` | `Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8` | LLM model |
| `TV_DEFAULT_CITY` | `Ferizaj` | weather fallback city |

## Deploy (CapRover → tv.lisaos.dev)
`captain-definition` + `Dockerfile` are at the repo root, so it deploys like every other
Construct service:
```
./deploy.sh                       # caprover deploy --appName tv --tarFile …
# or from the repo root:  caprover deploy --appName tv
```
Then set the env vars above in CapRover → Apps → tv → App Configs.

## Client
The display is an Android TV WebView kiosk (bundle id `space.construct.tv`) pointed at
this service. Voice in = the remote **OK button → Google STT**; replies via the browser's
SpeechSynthesis. (App lives in the Construct monorepo: `apps/construct_tv/androidtv`.)
