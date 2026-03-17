package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type upstreamCounters struct {
	mu     sync.Mutex
	rsc    int
	models int
	create int
	turn   int
}

func (c *upstreamCounters) incRSC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rsc++
}

func (c *upstreamCounters) incModels() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models++
}

func (c *upstreamCounters) incCreate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.create++
}

func (c *upstreamCounters) incTurn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turn++
}

func (c *upstreamCounters) snapshot() (rsc, models, create, turn int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rsc, c.models, c.create, c.turn
}

func newTestApp(t *testing.T) (*app, *upstreamCounters, func()) {
	t.Helper()

	counters := &upstreamCounters{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" && r.URL.Query().Get("_rsc") == "1h3ay":
			counters.incRSC()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/internal/experts/assistant/playground-models":
			counters.incModels()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"m1","endpointString":"qwen3-omni","displayName":"Qwen3 Omni","status":"Ok","visibility":"Public","provider":"outlier","inputTypes":["Text"],"outputTypes":["Text"]}]`))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/internal/experts/assistant/conversations":
			counters.incCreate()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"conv-test"}`))
			return
		case r.Method == http.MethodPost && (r.URL.Path == "/internal/experts/assistant/conversations/conv-test/turn-streaming" || r.URL.Path == "/internal/experts/assistant/conversations/custom-conv/turn-streaming"):
			counters.incTurn()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1773719000,\"model\":\"qwen3-omni\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		default:
			http.NotFound(w, r)
		}
	}))

	cfg := &config{
		ListenAddr:     ":0",
		BaseURL:        srv.URL,
		Cookie:         "_jwt=test",
		UserAgent:      defaultUA,
		Origin:         srv.URL,
		Referer:        srv.URL + "/",
		Timeout:        15 * time.Second,
		ModelCacheFile: filepath.Join(t.TempDir(), "models_cache.json"),
		ModelCacheTTL:  0,
		NewConvRSC:     "1h3ay",
		BlockingMode:   false,
		RPM:            0,
	}
	return newApp(cfg), counters, srv.Close
}

func postChat(t *testing.T, a *app, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	a.chatCompletionsHandler(rr, req)
	return rr
}

func TestPreflightCalledForNewConversation(t *testing.T) {
	a, counters, cleanup := newTestApp(t)
	defer cleanup()

	rr := postChat(t, a, map[string]any{
		"model": "qwen3-omni",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	rsc, _, create, turn := counters.snapshot()
	if rsc != 1 {
		t.Fatalf("expected preflight rsc call once, got %d", rsc)
	}
	if create != 1 || turn != 1 {
		t.Fatalf("expected create=1 and turn=1, got create=%d turn=%d", create, turn)
	}
}

func TestPreflightSkippedWhenMemoryTrue(t *testing.T) {
	a, counters, cleanup := newTestApp(t)
	defer cleanup()

	rr := postChat(t, a, map[string]any{
		"model":  "qwen3-omni",
		"memory": true,
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	rsc, _, create, turn := counters.snapshot()
	if rsc != 0 {
		t.Fatalf("expected preflight rsc call skipped for memory request, got %d", rsc)
	}
	if create != 1 || turn != 1 {
		t.Fatalf("expected create=1 and turn=1, got create=%d turn=%d", create, turn)
	}
}

func TestPreflightSkippedWhenConversationIDProvided(t *testing.T) {
	a, counters, cleanup := newTestApp(t)
	defer cleanup()

	rr := postChat(t, a, map[string]any{
		"model":           "qwen3-omni",
		"conversation_id": "custom-conv",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	rsc, _, create, turn := counters.snapshot()
	if rsc != 0 {
		t.Fatalf("expected preflight rsc call skipped for existing conversation, got %d", rsc)
	}
	if create != 0 || turn != 1 {
		t.Fatalf("expected create=0 and turn=1, got create=%d turn=%d", create, turn)
	}
}

func TestModelCacheNeverExpiresWhenTTLUnset(t *testing.T) {
	cache := newModelCache(0)
	cache.setWithTime(map[string]outlierModel{
		"m1": {ID: "m1", Endpoint: "m1"},
	}, time.Now().Add(-24*time.Hour))

	if cache.stale() {
		t.Fatalf("expected cache not stale when ttl=0 and models exist")
	}
}

func TestAuthHeadersReloadedFromEnvFile(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("OUTLIER_COOKIE=\"cookie-one\"\nOUTLIER_USER_AGENT=\"UA-ONE\"\n"), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	var (
		mu      sync.Mutex
		cookies []string
		uas     []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cookies = append(cookies, r.Header.Get("Cookie"))
		uas = append(uas, r.Header.Get("User-Agent"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := &config{
		ListenAddr:     ":0",
		BaseURL:        srv.URL,
		EnvFile:        envFile,
		Cookie:         "fallback-cookie",
		UserAgent:      "fallback-ua",
		Origin:         srv.URL,
		Referer:        srv.URL + "/",
		Timeout:        15 * time.Second,
		ModelCacheFile: filepath.Join(t.TempDir(), "models_cache.json"),
		ModelCacheTTL:  0,
		NewConvRSC:     "1h3ay",
	}
	client := newOutlierClient(cfg)

	resp1, err := client.doRaw(context.Background(), http.MethodGet, "/probe-1", nil)
	if err != nil {
		t.Fatalf("first doRaw: %v", err)
	}
	_ = resp1.Body.Close()

	if err := os.WriteFile(envFile, []byte("OUTLIER_COOKIE=\"cookie-two\"\nOUTLIER_USER_AGENT=\"UA-TWO\"\n"), 0o644); err != nil {
		t.Fatalf("rewrite env file: %v", err)
	}

	resp2, err := client.doRaw(context.Background(), http.MethodGet, "/probe-2", nil)
	if err != nil {
		t.Fatalf("second doRaw: %v", err)
	}
	_ = resp2.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(cookies) != 2 || len(uas) != 2 {
		t.Fatalf("expected 2 captured requests, got cookies=%d uas=%d", len(cookies), len(uas))
	}
	if cookies[0] != "cookie-one" || uas[0] != "UA-ONE" {
		t.Fatalf("unexpected first auth headers: cookie=%q ua=%q", cookies[0], uas[0])
	}
	if cookies[1] != "cookie-two" || uas[1] != "UA-TWO" {
		t.Fatalf("unexpected second auth headers: cookie=%q ua=%q", cookies[1], uas[1])
	}
}

func TestRPMExceededReturns429(t *testing.T) {
	a, counters, cleanup := newTestApp(t)
	defer cleanup()
	a.cfg.RPM = 1
	a.session = newSessionState(a.cfg)

	payload := map[string]any{
		"model":           "qwen3-omni",
		"conversation_id": "custom-conv",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}

	rr1 := postChat(t, a, payload)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request unexpected status: %d body=%s", rr1.Code, rr1.Body.String())
	}

	rr2 := postChat(t, a, payload)
	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request expected 429, got %d body=%s", rr2.Code, rr2.Body.String())
	}
	if rr2.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header for 429 response")
	}

	_, _, _, turn := counters.snapshot()
	if turn != 1 {
		t.Fatalf("expected only first request to reach turn-streaming, got turn=%d", turn)
	}
}

func TestBlockingModeSerializesTurnRequests(t *testing.T) {
	var (
		mu          sync.Mutex
		activeTurns int
		maxTurns    int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/internal/experts/assistant/playground-models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"m1","endpointString":"qwen3-omni","displayName":"Qwen3 Omni","status":"Ok","visibility":"Public","provider":"outlier","inputTypes":["Text"],"outputTypes":["Text"]}]`))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/internal/experts/assistant/conversations/custom-conv/turn-streaming":
			mu.Lock()
			activeTurns++
			if activeTurns > maxTurns {
				maxTurns = activeTurns
			}
			mu.Unlock()

			time.Sleep(120 * time.Millisecond)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-test\",\"object\":\"chat.completion.chunk\",\"created\":1773719000,\"model\":\"qwen3-omni\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))

			mu.Lock()
			activeTurns--
			mu.Unlock()
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config{
		ListenAddr:     ":0",
		BaseURL:        srv.URL,
		Cookie:         "_jwt=test",
		UserAgent:      defaultUA,
		Origin:         srv.URL,
		Referer:        srv.URL + "/",
		Timeout:        15 * time.Second,
		ModelCacheFile: filepath.Join(t.TempDir(), "models_cache.json"),
		ModelCacheTTL:  0,
		NewConvRSC:     "1h3ay",
		BlockingMode:   true,
		RPM:            0,
	}
	a := newApp(cfg)

	payload := map[string]any{
		"model":           "qwen3-omni",
		"conversation_id": "custom-conv",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}

	start := make(chan struct{})
	res := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rr := postChat(t, a, payload)
			res <- rr.Code
		}()
	}
	close(start)
	wg.Wait()
	close(res)

	for code := range res {
		if code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", code)
		}
	}
	if maxTurns != 1 {
		t.Fatalf("expected turn-streaming requests to be serialized, max concurrent=%d", maxTurns)
	}
}
