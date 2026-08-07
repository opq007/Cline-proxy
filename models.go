package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ModelStatus string

const (
	ModelActive  ModelStatus = "active"
	ModelRemoved ModelStatus = "removed"
	ModelUnknown ModelStatus = "unknown"
)

type ModelInfo struct {
	ID              string      `json:"id"`
	Name            string      `json:"name,omitempty"`
	Source          string      `json:"source"`
	Provider        string      `json:"provider"`
	Cost            string      `json:"cost"`
	Status          ModelStatus `json:"status"`
	RequiresStream  bool        `json:"requiresStream,omitempty"`
	ContextWindow   int         `json:"contextWindow,omitempty"`   // 输入上下文窗口（token），非 max_tokens
	MaxOutputTokens int         `json:"maxOutputTokens,omitempty"` // 单次生成上限（对应请求 max_tokens）
	SyncedAt        time.Time   `json:"syncedAt,omitempty"`
}

var (
	modelsMu       sync.Mutex
	modelsCache    map[string]*ModelInfo
	modelsSyncing  bool
	modelsLastSync time.Time
)

const (
	modelsRefreshInterval = 60 * time.Minute
	modelsSyncTimeout     = 25 * time.Second
	// DeepSeek V4 / GLM 5.2 官方上下文约 1M；单次 max 输出常见约 128K–131K。
	defaultContextWindow   = 1_000_000
	defaultMaxOutputTokens = 131_072
)

const recommendedModelsURL = clineAPIBase + "/ai/cline/recommended-models"

func seedModelCandidates() []*ModelInfo {
	// 离线兜底：仅放「当前已知可用」的 free 模型。
	// 新 free 模型（含未来官方重新开放的 cline-free/glm-5.2）只依赖 recommended-models 同步加入，
	// 切勿在此强行 seed 上游 404 的 ID，也勿用永久黑名单拦截官方 feed。
	return []*ModelInfo{
		{
			ID: "deepseek/deepseek-v4-flash", Source: "free", Provider: "deepseek", Cost: "free",
			RequiresStream: true, ContextWindow: defaultContextWindow, MaxOutputTokens: defaultMaxOutputTokens,
		},
		{
			ID: "poolside/laguna-s-2.1:free", Source: "free", Provider: "poolside", Cost: "free",
			ContextWindow: 128_000, MaxOutputTokens: 32_768,
		},
		{
			ID: "stepfun/step-3.7-flash", Source: "free", Provider: "stepfun", Cost: "free",
			RequiresStream: true, ContextWindow: 256_000, MaxOutputTokens: 65_536,
		},
	}
}

func initModelsCache() {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if modelsCache != nil {
		// 热更新：补全 seed 元数据；官方 sync 写入的模型原样保留（扩展性）
		for _, seed := range seedModelCandidates() {
			if existing, ok := modelsCache[seed.ID]; ok {
				if existing.ContextWindow <= 0 {
					existing.ContextWindow = seed.ContextWindow
				}
				if existing.MaxOutputTokens <= 0 {
					existing.MaxOutputTokens = seed.MaxOutputTokens
				}
				continue
			}
			cp := *seed
			if cp.Status == "" {
				cp.Status = ModelActive
			}
			modelsCache[seed.ID] = &cp
		}
		return
	}
	modelsCache = make(map[string]*ModelInfo)
	for _, m := range seedModelCandidates() {
		cp := *m
		if cp.Status == "" {
			cp.Status = ModelActive
		}
		modelsCache[cp.ID] = &cp
	}
}

func getFreeModels() []*ModelInfo {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	out := make([]*ModelInfo, 0, len(modelsCache))
	for _, m := range modelsCache {
		cp := *m
		out = append(out, &cp)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j-1].ID < out[j].ID {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

type recommendedPayload struct {
	Free []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"free"`
}

func syncRecommendedModels() (int, error) {
	initModelsCache()

	acc := pickAccount()
	if acc == nil {
		return 0, fmt.Errorf("no active accounts")
	}
	token, err := ensureAccountToken(acc)
	if err != nil {
		return 0, err
	}

	// 必须走该账号绑定的出口代理，避免模型清单请求暴露宿主机 IP。
	client, err := clientForAccount(acc)
	if err != nil {
		return 0, fmt.Errorf("account proxy: %w", err)
	}
	// 为同步请求加超时，不污染全局连接池上的默认 client。
	syncClient := *client
	syncClient.Timeout = modelsSyncTimeout

	req, err := http.NewRequest("GET", recommendedModelsURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header = clineHeaders(token, "")
	req.Header.Set("X-Task-ID", fmt.Sprintf("sess_sync_%d", time.Now().UnixMilli()))

	log.Printf("  model sync: fetching free list via account=%s proxy=%s",
		truncateEmail(acc.Email), proxyLabel(acc.ProxyURL))

	resp, err := syncClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var payload recommendedPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}

	modelsMu.Lock()
	defer modelsMu.Unlock()

	// seed 兜底始终存在；官方 free[] 为扩展来源（含未来新模型 / 重新上架的 ID）
	for _, seed := range seedModelCandidates() {
		if _, ok := modelsCache[seed.ID]; !ok {
			cp := *seed
			cp.Status = ModelActive
			modelsCache[seed.ID] = &cp
		}
	}

	added := 0
	for _, m := range payload.Free {
		id := m.ID
		provider := id
		if i := indexByte(id, '/'); i >= 0 {
			provider = id[:i]
		}
		if cached, ok := modelsCache[id]; ok {
			cached.Source = "free"
			cached.Cost = "free"
			cached.Provider = provider
			cached.Status = ModelActive
			cached.SyncedAt = time.Now()
			if cached.Name == "" {
				cached.Name = m.Name
			}
			if cached.ContextWindow <= 0 {
				cached.ContextWindow = inferContextWindow(id)
			}
			if cached.MaxOutputTokens <= 0 {
				cached.MaxOutputTokens = inferMaxOutputTokens(id)
			}
			continue
		}
		modelsCache[id] = &ModelInfo{
			ID:              id,
			Name:            m.Name,
			Source:          "free",
			Provider:        provider,
			Cost:            "free",
			Status:          ModelActive,
			RequiresStream:  indexByte(id, ':') < 0,
			ContextWindow:   inferContextWindow(id),
			MaxOutputTokens: inferMaxOutputTokens(id),
			SyncedAt:        time.Now(),
		}
		added++
	}

	modelsLastSync = time.Now()
	return added, nil
}

func inferContextWindow(id string) int {
	lid := strings.ToLower(id)
	switch {
	case strings.Contains(lid, "deepseek-v4"), strings.Contains(lid, "glm-5.2"), strings.Contains(lid, "glm-5"):
		return defaultContextWindow
	case strings.Contains(lid, "step-3"):
		return 256_000
	default:
		return 128_000
	}
}

func inferMaxOutputTokens(id string) int {
	lid := strings.ToLower(id)
	switch {
	case strings.Contains(lid, "deepseek-v4"), strings.Contains(lid, "glm-5.2"), strings.Contains(lid, "glm-5"):
		return defaultMaxOutputTokens
	case strings.Contains(lid, "step-3"):
		return 65_536
	default:
		return 32_768
	}
}

// modelMaxOutputTokens 请求默认 max_tokens（生成上限），不是上下文窗口。
func modelMaxOutputTokens(modelID string) int {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if m, ok := modelsCache[modelID]; ok && m.MaxOutputTokens > 0 {
		return m.MaxOutputTokens
	}
	return inferMaxOutputTokens(modelID)
}

// modelContextWindow 模型输入上下文窗口（token）。
func modelContextWindow(modelID string) int {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if m, ok := modelsCache[modelID]; ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return inferContextWindow(modelID)
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func syncModelsOnce() {
	initModelsCache()
	modelsMu.Lock()
	if modelsSyncing {
		modelsMu.Unlock()
		return
	}
	modelsSyncing = true
	modelsMu.Unlock()
	defer func() {
		modelsMu.Lock()
		modelsSyncing = false
		modelsMu.Unlock()
	}()

	added, err := syncRecommendedModels()
	if err != nil {
		log.Printf("  model sync: failed (%v), using cached list", err)
		return
	}
	if added > 0 {
		log.Printf("  model sync: %d new free models from official feed", added)
	} else {
		log.Printf("  model sync: %d free models up to date", len(getFreeModels()))
	}
}

func getDefaultModel() string {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()

	if m, ok := modelsCache[defaultModel]; ok && m.Status == ModelActive {
		return defaultModel
	}
	for _, m := range modelsCache {
		if m.Status == ModelActive {
			return m.ID
		}
	}
	return defaultModel
}

func normalizeRequestModel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return getDefaultModel()
	}
	initModelsCache()
	modelsMu.Lock()
	_, ok := modelsCache[id]
	modelsMu.Unlock()
	if ok {
		return id
	}
	log.Printf("  model %q not in free list, fallback to %q", id, getDefaultModel())
	return getDefaultModel()
}

func apiModelList() []map[string]any {
	out := make([]map[string]any, 0, len(modelsCache))
	for _, m := range getFreeModels() {
		ctx := m.ContextWindow
		if ctx <= 0 {
			ctx = inferContextWindow(m.ID)
		}
		maxOut := m.MaxOutputTokens
		if maxOut <= 0 {
			maxOut = inferMaxOutputTokens(m.ID)
		}
		out = append(out, map[string]any{
			"id":               m.ID,
			"object":           "model",
			"created":          time.Now().UnixMilli(),
			"owned_by":         m.Provider,
			"source":           m.Source,
			"status":           m.Status,
			"cost":             m.Cost,
			"requiresStream":   m.RequiresStream,
			"context_window":   ctx,
			"contextWindow":    ctx,
			"max_output_tokens": maxOut,
			"maxOutputTokens":  maxOut,
			"syncedAt":         m.SyncedAt,
		})
	}
	return out
}

func ensureModelsFresh() {
	initModelsCache()
	modelsMu.Lock()
	needSync := modelsLastSync.IsZero() || time.Since(modelsLastSync) > modelsRefreshInterval
	syncing := modelsSyncing
	modelsMu.Unlock()
	if needSync && !syncing {
		go syncModelsOnce()
	}
}

func startModelsRefresher() {
	go func() {
		syncModelsOnce()
		ticker := time.NewTicker(modelsRefreshInterval)
		for range ticker.C {
			syncModelsOnce()
		}
	}()
}
