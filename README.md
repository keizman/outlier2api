# outlier-openai-proxy

Minimal OpenAI-compatible API adapter for Outlier Playground web session.

Supported endpoints:

- `GET /v1/models`
- `POST /v1/chat/completions` (`stream: true/false`)
- `GET/POST /v1/updatemodels` (force refresh model cache)
- `GET/POST /updatemodels` (alias)

This project is intentionally small and does not include Docker/compose.

## 1. Requirements

- Go 1.22+
- A valid logged-in Outlier browser session

## 2. Environment Variables

`main.go` will auto-load `.env` from current directory (if present).

Required:

- `OUTLIER_COOKIE`  
  Raw browser `Cookie` header value for `https://playground.outlier.ai`.

Optional:

- `LISTEN_ADDR` (default `:8080`)
- `OUTLIER_BASE_URL` (default `https://playground.outlier.ai`)
- `OUTLIER_USER_AGENT` (default Chrome 146 UA)
- `OUTLIER_ORIGIN` (default same as base URL)
- `OUTLIER_REFERER` (default `<base>/`)
- `OUTLIER_MODELS_CACHE_FILE` (default `models_cache.json`)
- `OUTLIER_MODELS_CACHE_TTL` (optional; empty/unset means never expire; format like `30m`, `2h`, `24h`)
- `OUTLIER_NEW_CONV_RSC` (default `1h3ay`, used for `GET /?_rsc=<value>` preflight on new conversation)

Files in this repo:

- `.env.example` template
- `.env` local runtime config

## 3. Manual Cookie / UA Extraction

1. Open `https://playground.outlier.ai` in your logged-in browser.
2. Open DevTools -> `Network`.
3. Send one test prompt.
4. Click request `POST /internal/experts/assistant/conversations`.
5. In `Request Headers`:
   - copy `cookie` as `OUTLIER_COOKIE`
   - copy `user-agent` as `OUTLIER_USER_AGENT` (optional but recommended)
6. Paste values into `.env`.

## 4. Run

```powershell
cd E:\git\LLM\outlier-openai-proxy
go run .
```

Server starts at `http://localhost:8080` by default.

## 5. OpenAI-Compatible Usage

### models

```bash
curl http://localhost:8080/v1/models
```

### force refresh models cache

```bash
curl -X POST http://localhost:8080/v1/updatemodels
```

### non-stream chat

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model":"qwen3-235b-a22b-2507-v1",
    "messages":[
      {"role":"system","content":"You are concise."},
      {"role":"user","content":"Hello"}
    ]
  }'
```

### stream chat

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model":"qwen3-235b-a22b-2507-v1",
    "stream": true,
    "messages":[{"role":"user","content":"Write 3 bullet points"}]
  }'
```

### memory mode (extension field)

If `memory=true`, service skips the new-conversation `?_rsc` preflight request.

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model":"qwen3-235b-a22b-2507-v1",
    "memory": true,
    "messages":[{"role":"user","content":"remember this"}]
  }'
```

## 6. Models Cache Behavior

- `/v1/models` first uses memory cache.
- If memory cache is stale/empty, service attempts local cache file (`OUTLIER_MODELS_CACHE_FILE`).
- If local cache is missing, it fetches from Outlier and writes file.
- If `OUTLIER_MODELS_CACHE_TTL` is unset/empty, cache never expires automatically.
- `updatemodels` always forces upstream fetch and overwrites local cache file.

## 7. New Conversation Preflight Behavior

- On first turn of a new conversation (`conversation_id` missing), proxy sends one best-effort request:
  - `GET /?_rsc=<OUTLIER_NEW_CONV_RSC>`
- On continued chat (`conversation_id` provided), proxy does not send it.
- If `memory=true`, proxy does not send it.
- If preflight fails, chat request continues (preflight is auxiliary).

## 8. Session Expiration and Cookie Rotation

- There is no fixed hardcoded expiration in this proxy.
- Real expiry is controlled by Outlier cookies/session on server side.
- In practice, when cookie expires or is invalidated, upstream calls return auth errors (commonly 401/403-like behavior).

How to inspect cookie expiry:

1. DevTools -> Application -> Cookies -> `https://playground.outlier.ai`
2. Check `Expires / Max-Age` for session-related cookies
3. If `Session` type cookie (no explicit Expires), it usually expires when browser session ends or server invalidates it

### Auto refresh script (Playwright MCP extension)

Script path:

- `scripts/refresh_cookie_mcp.py`

Prerequisites:

- Chrome/Edge has **Playwright MCP Bridge** extension installed.
- MCP extension token is available as env var:
  - `PLAYWRIGHT_MCP_EXTENSION_TOKEN=...`
- Python dependency:
  - `pip install requests`

One-time refresh:

```bash
python scripts/refresh_cookie_mcp.py --env .env --token "$PLAYWRIGHT_MCP_EXTENSION_TOKEN"
```

Daemon mode (refresh before expiry):

```bash
python scripts/refresh_cookie_mcp.py --daemon --env .env --token "$PLAYWRIGHT_MCP_EXTENSION_TOKEN" --lead-seconds 3600
```

This script updates:

- `OUTLIER_COOKIE`
- `OUTLIER_USER_AGENT`

## 9. How Reference Project Handles Session

Based on `yushangxiao/claude2api`:

- It reads `SESSIONS` (sessionKey list) from env/YAML.
- It does retry/round-robin across provided session keys.
- It does not perform browser login automation or automatic credential refresh.
- Operationally, invalid sessions must still be replaced by user/operator.

## 10. Compliance Reminder

Use this only with accounts and data you are authorized to access, and follow the target platform's terms and policies.
# outlier2api
