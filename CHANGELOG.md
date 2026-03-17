# Changelog

## 2026-03-17

- Switched upstream HTTP client to `github.com/imroc/req/v3` with `ImpersonateChrome()` and browser-like default headers.
- Added dynamic auth header reload from `.env` on every upstream request (`OUTLIER_COOKIE`, `OUTLIER_USER_AGENT`).
- Added models cache persistence with optional TTL (`OUTLIER_MODELS_CACHE_FILE`, `OUTLIER_MODELS_CACHE_TTL`; empty TTL means no expiry).
- Added new-conversation preflight request (`GET /?_rsc=<value>`) for non-memory first turns (`OUTLIER_NEW_CONV_RSC`).
- Added optional blocking mode for chat requests using semaphore serialization (`OUTLIER_BLOCKING_MODE`).
- Added optional request-per-minute guard for chat requests with `429` + `Retry-After` (`OUTLIER_RPM`).
