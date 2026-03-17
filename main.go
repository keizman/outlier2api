package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://playground.outlier.ai"
	defaultUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
)

type config struct {
	ListenAddr     string
	BaseURL        string
	EnvFile        string
	Cookie         string
	UserAgent      string
	Origin         string
	Referer        string
	Timeout        time.Duration
	ModelCacheFile string
	ModelCacheTTL  time.Duration
	NewConvRSC     string
}

func loadConfig() (*config, error) {
	c := &config{
		ListenAddr:     getEnv("LISTEN_ADDR", ":8080"),
		BaseURL:        strings.TrimRight(getEnv("OUTLIER_BASE_URL", defaultBaseURL), "/"),
		EnvFile:        strings.TrimSpace(getEnv("OUTLIER_ENV_FILE", ".env")),
		Cookie:         strings.TrimSpace(os.Getenv("OUTLIER_COOKIE")),
		UserAgent:      getEnv("OUTLIER_USER_AGENT", defaultUA),
		Timeout:        90 * time.Second,
		ModelCacheFile: getEnv("OUTLIER_MODELS_CACHE_FILE", "models_cache.json"),
		ModelCacheTTL:  parseDurationEnv("OUTLIER_MODELS_CACHE_TTL", 0),
		NewConvRSC:     strings.TrimSpace(getEnv("OUTLIER_NEW_CONV_RSC", "1h3ay")),
	}

	if c.Cookie == "" {
		return nil, errors.New("OUTLIER_COOKIE is required")
	}

	c.Origin = getEnv("OUTLIER_ORIGIN", c.BaseURL)
	c.Referer = getEnv("OUTLIER_REFERER", c.BaseURL+"/")
	if c.Origin == "" {
		c.Origin = c.BaseURL
	}
	if c.Referer == "" {
		c.Referer = c.BaseURL + "/"
	}
	return c, nil
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("invalid %s=%q, fallback to %s", key, raw, fallback)
		return fallback
	}
	return d
}

func formatTTL(d time.Duration) string {
	if d <= 0 {
		return "never"
	}
	return d.String()
}

func parseDotEnvValue(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		val := parseDotEnvValue(parts[1])
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return sc.Err()
}

func readDotEnvAuth(path string) (cookie string, userAgent string, err error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		return "", "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if key == "" {
			continue
		}
		val := parseDotEnvValue(parts[1])
		switch key {
		case "OUTLIER_COOKIE":
			cookie = strings.TrimSpace(val)
		case "OUTLIER_USER_AGENT":
			userAgent = strings.TrimSpace(val)
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	return cookie, userAgent, nil
}

type outlierModel struct {
	ID           string   `json:"id"`
	Endpoint     string   `json:"endpointString"`
	DisplayName  string   `json:"displayName"`
	Status       string   `json:"status"`
	Visibility   string   `json:"visibility"`
	Provider     string   `json:"provider"`
	ParentID     *string  `json:"parentId"`
	InputTypes   []string `json:"inputTypes"`
	OutputTypes  []string `json:"outputTypes"`
	ModelVersion string   `json:"modelVersion"`
}

type modelEntry struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	OwnedBy    string `json:"owned_by"`
	Permission []any  `json:"permission"`
	Root       string `json:"root,omitempty"`
	Parent     string `json:"parent,omitempty"`
}

type modelListResponse struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

type persistedModelCache struct {
	UpdatedAt  time.Time               `json:"updated_at"`
	ByEndpoint map[string]outlierModel `json:"by_endpoint"`
}

type modelCache struct {
	mu          sync.RWMutex
	lastRefresh time.Time
	ttl         time.Duration
	byEndpoint  map[string]outlierModel
}

func newModelCache(ttl time.Duration) *modelCache {
	return &modelCache{
		ttl:        ttl,
		byEndpoint: make(map[string]outlierModel),
	}
}

func (m *modelCache) getSnapshot() map[string]outlierModel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make(map[string]outlierModel, len(m.byEndpoint))
	for k, v := range m.byEndpoint {
		cp[k] = v
	}
	return cp
}

func (m *modelCache) setWithTime(models map[string]outlierModel, refreshTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byEndpoint = models
	if refreshTime.IsZero() {
		refreshTime = time.Now()
	}
	m.lastRefresh = refreshTime
}

func (m *modelCache) set(models map[string]outlierModel) {
	m.setWithTime(models, time.Now())
}

func (m *modelCache) stale() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.byEndpoint) == 0 {
		return true
	}
	if m.ttl <= 0 {
		return false
	}
	return time.Since(m.lastRefresh) > m.ttl
}

func (m *modelCache) empty() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byEndpoint) == 0
}

func (m *modelCache) meta() (int, time.Time) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byEndpoint), m.lastRefresh
}

type outlierClient struct {
	cfg        *config
	httpClient *http.Client
	models     *modelCache
	authMu     sync.RWMutex
	authCookie string
	authUA     string
}

func newOutlierClient(cfg *config) *outlierClient {
	tr := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100,
		MaxConnsPerHost:     100,
		IdleConnTimeout:     120 * time.Second,
	}
	return &outlierClient{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: tr,
		},
		models:     newModelCache(cfg.ModelCacheTTL),
		authCookie: cfg.Cookie,
		authUA:     cfg.UserAgent,
	}
}

func (c *outlierClient) refreshAuthFromEnvFile() {
	envPath := strings.TrimSpace(c.cfg.EnvFile)
	if envPath == "" {
		return
	}
	cookie, ua, err := readDotEnvAuth(envPath)
	if err != nil {
		log.Printf("auth reload warning: failed to read %s: %v", envPath, err)
		return
	}
	if cookie == "" && ua == "" {
		return
	}

	c.authMu.Lock()
	if cookie != "" {
		c.authCookie = cookie
	}
	if ua != "" {
		c.authUA = ua
	}
	c.authMu.Unlock()
}

func (c *outlierClient) authValues() (cookie string, userAgent string) {
	c.refreshAuthFromEnvFile()
	c.authMu.RLock()
	defer c.authMu.RUnlock()
	return c.authCookie, c.authUA
}

func (c *outlierClient) ensureModelCache(ctx context.Context) error {
	if !c.models.stale() {
		return nil
	}

	// Try local file cache first if memory cache is empty.
	if c.models.empty() {
		if err := c.loadModelsFromFile(false); err == nil && !c.models.stale() {
			return nil
		}
	}

	models, err := c.fetchModels(ctx)
	if err != nil {
		// Fallback to stale local cache if available.
		if loadErr := c.loadModelsFromFile(true); loadErr == nil && !c.models.empty() {
			log.Printf("use stale models cache from file due to upstream error: %v", err)
			return nil
		}
		return err
	}
	c.models.set(models)
	if err := c.saveModelsToFile(models); err != nil {
		log.Printf("save models cache failed: %v", err)
	}
	return nil
}

func (c *outlierClient) forceRefreshModels(ctx context.Context) (int, error) {
	models, err := c.fetchModels(ctx)
	if err != nil {
		return 0, err
	}
	c.models.set(models)
	if err := c.saveModelsToFile(models); err != nil {
		log.Printf("save models cache failed: %v", err)
	}
	return len(models), nil
}

func (c *outlierClient) loadModelsFromFile(allowStale bool) error {
	path := strings.TrimSpace(c.cfg.ModelCacheFile)
	if path == "" {
		return errors.New("models cache file path is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var persisted persistedModelCache
	if err := json.Unmarshal(b, &persisted); err != nil {
		return err
	}
	if len(persisted.ByEndpoint) == 0 {
		return errors.New("models cache file has no models")
	}
	refreshAt := persisted.UpdatedAt
	if refreshAt.IsZero() {
		refreshAt = time.Now()
	}
	c.models.setWithTime(persisted.ByEndpoint, refreshAt)
	if !allowStale && c.models.stale() {
		return errors.New("models cache file is stale")
	}
	return nil
}

func (c *outlierClient) saveModelsToFile(models map[string]outlierModel) error {
	path := strings.TrimSpace(c.cfg.ModelCacheFile)
	if path == "" {
		return nil
	}
	payload := persistedModelCache{
		UpdatedAt:  time.Now().UTC(),
		ByEndpoint: models,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, b, 0o644)
}

func (c *outlierClient) preflightNewConversation(ctx context.Context) error {
	if strings.TrimSpace(c.cfg.NewConvRSC) == "" {
		return nil
	}
	path := "/?_rsc=" + url.QueryEscape(c.cfg.NewConvRSC)
	resp, err := c.doRaw(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("new conversation preflight failed: status=%d", resp.StatusCode)
	}
	return nil
}

func hasType(items []string, target string) bool {
	for _, s := range items {
		if strings.EqualFold(strings.TrimSpace(s), target) {
			return true
		}
	}
	return false
}

func chooseModel(existing outlierModel, candidate outlierModel) outlierModel {
	// Prefer non-variant root records (parentId == nil).
	if existing.ParentID != nil && candidate.ParentID == nil {
		return candidate
	}
	if existing.ParentID == nil && candidate.ParentID != nil {
		return existing
	}
	// Then prefer public-like visibility.
	score := func(v string) int {
		switch strings.ToLower(v) {
		case "public":
			return 4
		case "private":
			return 3
		case "experimental":
			return 2
		case "prerelease":
			return 1
		default:
			return 0
		}
	}
	if score(candidate.Visibility) > score(existing.Visibility) {
		return candidate
	}
	return existing
}

func (c *outlierClient) fetchModels(ctx context.Context) (map[string]outlierModel, error) {
	var raw []outlierModel
	if err := c.doJSON(ctx, http.MethodGet, "/internal/experts/assistant/playground-models", nil, &raw); err != nil {
		return nil, err
	}

	byEndpoint := make(map[string]outlierModel)
	for _, m := range raw {
		endpoint := strings.TrimSpace(m.Endpoint)
		if endpoint == "" {
			continue
		}
		if !strings.EqualFold(m.Status, "Ok") {
			continue
		}
		if !hasType(m.InputTypes, "Text") || !hasType(m.OutputTypes, "Text") {
			continue
		}
		if prev, ok := byEndpoint[endpoint]; ok {
			byEndpoint[endpoint] = chooseModel(prev, m)
		} else {
			byEndpoint[endpoint] = m
		}
	}

	if len(byEndpoint) == 0 {
		return nil, errors.New("no usable models found from playground-models")
	}
	return byEndpoint, nil
}

func (c *outlierClient) buildRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	url := c.cfg.BaseURL + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	cookie, ua := c.authValues()
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json, text/event-stream, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", c.cfg.Origin)
	req.Header.Set("Referer", c.cfg.Referer)
	req.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *outlierClient) doRaw(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	req, err := c.buildRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

func (c *outlierClient) doJSON(ctx context.Context, method, path string, payload any, out any) error {
	resp, err := c.doRaw(ctx, method, path, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("outlier %s %s failed: status=%d body=%s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []openAIMessage `json:"messages"`
	Stream         bool            `json:"stream,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"` // extension field
	Memory         bool            `json:"memory,omitempty"`          // extension field
}

type openAIErrorEnvelope struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type chatCompletionResponse struct {
	ID      string                         `json:"id"`
	Object  string                         `json:"object"`
	Created int64                          `json:"created"`
	Model   string                         `json:"model"`
	Choices []chatCompletionResponseChoice `json:"choices"`
}

type chatCompletionResponseChoice struct {
	Index        int         `json:"index"`
	Message      responseMsg `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type responseMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type sseChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

type app struct {
	cfg    *config
	client *outlierClient
}

func newApp(cfg *config) *app {
	return &app{
		cfg:    cfg,
		client: newOutlierClient(cfg),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, openAIErrorEnvelope{
		Error: openAIError{
			Message: msg,
			Type:    "invalid_request_error",
		},
	})
}

func extractTextContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var asParts []map[string]any
	if err := json.Unmarshal(raw, &asParts); err == nil {
		var b strings.Builder
		for _, p := range asParts {
			t, _ := p["type"].(string)
			if t != "" && t != "text" {
				continue
			}
			if text, ok := p["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

func extractPromptAndSystem(messages []openAIMessage) (prompt, system string) {
	systemParts := make([]string, 0, 2)
	for _, m := range messages {
		text := strings.TrimSpace(extractTextContent(m.Content))
		if text == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system":
			systemParts = append(systemParts, text)
		case "user":
			prompt = text // use latest user turn
		}
	}
	system = strings.Join(systemParts, "\n\n")
	return prompt, system
}

func (a *app) modelsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := a.client.ensureModelCache(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	snap := a.client.models.getSnapshot()
	ids := make([]string, 0, len(snap))
	for id := range snap {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := make([]modelEntry, 0, len(ids))
	for _, id := range ids {
		m := snap[id]
		data = append(data, modelEntry{
			ID:         id,
			Object:     "model",
			OwnedBy:    strings.ToLower(strings.TrimSpace(m.Provider)),
			Permission: []any{},
			Root:       id,
		})
	}
	writeJSON(w, http.StatusOK, modelListResponse{
		Object: "list",
		Data:   data,
	})
}

func (a *app) updateModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	count, err := a.client.forceRefreshModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_, refreshedAt := a.client.models.meta()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"count":            count,
		"cache_file":       a.cfg.ModelCacheFile,
		"cache_ttl":        formatTTL(a.cfg.ModelCacheTTL),
		"cache_refreshed":  refreshedAt.UTC().Format(time.RFC3339),
		"upstream_refetch": true,
	})
}

type createConversationPayload struct {
	Prompt struct {
		Text   string   `json:"text"`
		Images []string `json:"images"`
	} `json:"prompt"`
	Model           string `json:"model"`
	ModelID         string `json:"modelId"`
	ChallengeID     string `json:"challengeId"`
	InitialTurnMode string `json:"initialTurnMode"`
	InitialTurnType string `json:"initialTurnType"`
	IsMysteryModel  bool   `json:"isMysteryModel"`
}

type createConversationResponse struct {
	ID string `json:"id"`
}

type turnPrompt struct {
	Model            string   `json:"model"`
	TurnType         string   `json:"turnType"`
	ModelID          string   `json:"modelId"`
	Text             string   `json:"text"`
	Images           []string `json:"images"`
	ModelWasSwitched bool     `json:"modelWasSwitched"`
	IsMysteryModel   bool     `json:"isMysteryModel"`
	TurnMode         string   `json:"turnMode"`
	SystemMessage    string   `json:"systemMessage,omitempty"`
}

type turnStreamingPayload struct {
	Prompt         turnPrompt `json:"prompt"`
	Model          string     `json:"model"`
	ModelID        string     `json:"modelId"`
	TurnMode       string     `json:"turnMode"`
	TurnType       string     `json:"turnType"`
	IsMysteryModel bool       `json:"isMysteryModel"`
	SystemMessage  string     `json:"systemMessage,omitempty"`
	ParentIdx      *int       `json:"parentIdx,omitempty"`
}

func (a *app) createConversation(ctx context.Context, model, modelID, prompt string) (string, error) {
	payload := createConversationPayload{
		Model:           model,
		ModelID:         modelID,
		ChallengeID:     "",
		InitialTurnMode: "Normal",
		InitialTurnType: "Text",
		IsMysteryModel:  false,
	}
	payload.Prompt.Text = prompt
	payload.Prompt.Images = []string{}

	var out createConversationResponse
	if err := a.client.doJSON(ctx, http.MethodPost, "/internal/experts/assistant/conversations", payload, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", errors.New("conversation id missing in create response")
	}
	return out.ID, nil
}

func parseSSE(respBody io.Reader, onData func(data string) error) error {
	reader := bufio.NewReader(respBody)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if err := onData(data); err != nil {
				return err
			}
		}
	}
}

func aggregateSSEToText(respBody io.Reader) (id string, model string, created int64, content string, err error) {
	var b strings.Builder
	created = time.Now().Unix()
	err = parseSSE(respBody, func(data string) error {
		if data == "" || data == "[DONE]" {
			return nil
		}
		var chunk sseChunk
		if e := json.Unmarshal([]byte(data), &chunk); e != nil {
			return nil
		}
		if id == "" && chunk.ID != "" {
			id = chunk.ID
		}
		if model == "" && chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Created > 0 {
			created = chunk.Created
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				b.WriteString(ch.Delta.Content)
			}
		}
		return nil
	})
	content = b.String()
	return
}

func (a *app) relaySSE(w http.ResponseWriter, upstream io.ReadCloser) error {
	defer upstream.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	sawDone := false
	err := parseSSE(upstream, func(data string) error {
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			sawDone = true
		}
		if _, e := io.WriteString(w, "data: "+data+"\n\n"); e != nil {
			return e
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !sawDone {
		if _, e := io.WriteString(w, "data: [DONE]\n\n"); e != nil {
			return e
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	return nil
}

func (a *app) chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	defer r.Body.Close()

	var req chatCompletionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeErr(w, http.StatusBadRequest, "messages is required")
		return
	}

	prompt, systemMessage := extractPromptAndSystem(req.Messages)
	if strings.TrimSpace(prompt) == "" {
		writeErr(w, http.StatusBadRequest, "at least one user message with text content is required")
		return
	}

	ctx := r.Context()
	if err := a.client.ensureModelCache(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	models := a.client.models.getSnapshot()
	modelInfo, ok := models[req.Model]
	if !ok {
		writeErr(w, http.StatusBadRequest, "model not found in current Outlier model list")
		return
	}

	conversationID := strings.TrimSpace(req.ConversationID)
	if conversationID == "" {
		if !req.Memory {
			if err := a.client.preflightNewConversation(ctx); err != nil {
				// Best effort to mimic browser startup traffic; do not block chat if this auxiliary call fails.
				log.Printf("new conversation preflight warning: %v", err)
			}
		}

		id, err := a.createConversation(ctx, req.Model, modelInfo.ID, prompt)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		conversationID = id
	}

	turnPayload := turnStreamingPayload{
		Model:          req.Model,
		ModelID:        modelInfo.ID,
		TurnMode:       "Normal",
		TurnType:       "Text",
		IsMysteryModel: false,
		Prompt: turnPrompt{
			Model:            req.Model,
			TurnType:         "Text",
			ModelID:          modelInfo.ID,
			Text:             prompt,
			Images:           []string{},
			ModelWasSwitched: true,
			IsMysteryModel:   false,
			TurnMode:         "Normal",
		},
	}
	if strings.TrimSpace(systemMessage) != "" {
		turnPayload.SystemMessage = systemMessage
		turnPayload.Prompt.SystemMessage = systemMessage
	}

	path := fmt.Sprintf("/internal/experts/assistant/conversations/%s/turn-streaming", conversationID)
	upstreamResp, err := a.client.doRaw(ctx, http.MethodPost, path, turnPayload)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if upstreamResp.StatusCode < 200 || upstreamResp.StatusCode >= 300 {
		defer upstreamResp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(upstreamResp.Body, 4096))
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("turn-streaming failed: status=%d body=%s", upstreamResp.StatusCode, strings.TrimSpace(string(b))))
		return
	}

	if req.Stream {
		if err := a.relaySSE(w, upstreamResp.Body); err != nil {
			log.Printf("stream relay error: %v", err)
		}
		return
	}

	defer upstreamResp.Body.Close()
	id, modelFromChunk, created, answer, err := aggregateSSEToText(upstreamResp.Body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if strings.TrimSpace(id) == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	finalModel := req.Model
	if strings.TrimSpace(modelFromChunk) != "" {
		finalModel = modelFromChunk
	}
	writeJSON(w, http.StatusOK, chatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   finalModel,
		Choices: []chatCompletionResponseChoice{
			{
				Index: 0,
				Message: responseMsg{
					Role:    "assistant",
					Content: answer,
				},
				FinishReason: "stop",
			},
		},
	})
}

func (a *app) healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"time": time.Now().UTC().Format(time.RFC3339),
	})
}

func main() {
	if err := loadDotEnv(".env"); err != nil {
		log.Fatalf("failed to load .env: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	app := newApp(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.healthHandler)
	mux.HandleFunc("/v1/models", app.modelsHandler)
	mux.HandleFunc("/v1/updatemodels", app.updateModelsHandler)
	mux.HandleFunc("/updatemodels", app.updateModelsHandler)
	mux.HandleFunc("/v1/chat/completions", app.chatCompletionsHandler)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("outlier-openai-proxy listening on %s", cfg.ListenAddr)
	log.Printf("base=%s", cfg.BaseURL)
	log.Printf("env_file=%s (auth headers reload on each upstream request)", cfg.EnvFile)
	log.Printf("models_cache_file=%s ttl=%s", cfg.ModelCacheFile, formatTTL(cfg.ModelCacheTTL))
	log.Printf("new_conversation_rsc=%q", cfg.NewConvRSC)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
