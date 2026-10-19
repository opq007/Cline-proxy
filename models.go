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
	Note            string      `json:"note,omitempty"` // 最近一次状态变化的依据（如 404 / 促销结束 / 要求订阅）
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
	// 冷启动兜底：仅在缓存为空（离线/无账号）时提供可用入口。
	// 一旦官方同步成功，以下模型与 feed 一视同仁参与对账：不在 feed 中即下架。
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
		// 热更新：补全 seed 元数据；官方 sync 写入的模型原样保留（扩展性）。
		// 已下架（removed）的 seed 条目仍然存在，不会被这里复活。
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

// modelUsable 判断该状态是否可对客户端暴露/调用。
func modelUsable(s ModelStatus) bool {
	return s == ModelActive || s == ModelUnknown
}

// getFreeModels 客户端可见清单（/v1/models）：仅可用模型。
func getFreeModels() []*ModelInfo {
	models := allModelsLocked()
	out := make([]*ModelInfo, 0, len(models))
	for _, m := range models {
		if !modelUsable(m.Status) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// getAllModels 管理面板可见清单：包含已下架模型，便于观测。
func getAllModels() []*ModelInfo {
	return allModelsLocked()
}

func allModelsLocked() []*ModelInfo {
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

type recommendedModelEntry struct {
	ID   string
	Name string
}

// reconcileFeed 用官方 feed 对账模型缓存，feed 是唯一事实来源：
// feed 中出现的模型置为 active；缓存中不在 feed 里的模型（含 seed）置为 removed。
// 返回新增与下架数量。调用方必须已持有 modelsMu。
func reconcileFeed(cache map[string]*ModelInfo, feed []recommendedModelEntry) (added, removed int) {
	now := time.Now()
	feedSet := make(map[string]struct{}, len(feed))
	for _, m := range feed {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		feedSet[id] = struct{}{}
		provider := id
		if i := indexByte(id, '/'); i >= 0 {
			provider = id[:i]
		}
		if cached, ok := cache[id]; ok {
			cached.Source = "free"
			cached.Cost = "free"
			cached.Provider = provider
			cached.Status = ModelActive
			cached.SyncedAt = now
			cached.Note = ""
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
		cache[id] = &ModelInfo{
			ID:              id,
			Name:            m.Name,
			Source:          "free",
			Provider:        provider,
			Cost:            "free",
			Status:          ModelActive,
			RequiresStream:  indexByte(id, ':') < 0,
			ContextWindow:   inferContextWindow(id),
			MaxOutputTokens: inferMaxOutputTokens(id),
			SyncedAt:        now,
		}
		added++
	}
	for id, m := range cache {
		if _, ok := feedSet[id]; ok {
			continue
		}
		if m.Status != ModelRemoved {
			m.Status = ModelRemoved
			m.SyncedAt = now
			m.Note = "不在官方免费清单中，已对账下架"
			removed++
		}
	}
	return added, removed
}

func syncRecommendedModels() (added, removed int, err error) {
	initModelsCache()

	acc := pickAccount()
	if acc == nil {
		return 0, 0, fmt.Errorf("no active accounts")
	}
	token, err := ensureAccountToken(acc)
	if err != nil {
		return 0, 0, err
	}

	// 必须走该账号绑定的出口代理，避免模型清单请求暴露宿主机 IP。
	client, err := clientForAccount(acc)
	if err != nil {
		return 0, 0, fmt.Errorf("account proxy: %w", err)
	}
	// 为同步请求加超时，不污染全局连接池上的默认 client。
	syncClient := *client
	syncClient.Timeout = modelsSyncTimeout

	req, err := http.NewRequest("GET", recommendedModelsURL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header = clineHeaders(token, "")
	req.Header.Set("X-Task-ID", fmt.Sprintf("sess_sync_%d", time.Now().UnixMilli()))

	log.Printf("  model sync: fetching free list via account=%s proxy=%s",
		truncateEmail(acc.Email), proxyLabel(acc.ProxyURL))

	resp, err := syncClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var payload recommendedPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, 0, err
	}

	// 空 free[] 更可能是喂源异常而非「真的一条促销都没有」：跳过本轮对账，
	// 保留现有清单。漏网的陈旧条目会在真实调用 404 时被 noteModelCallFailure 下架。
	if len(payload.Free) == 0 {
		modelsMu.Lock()
		modelsLastSync = time.Now()
		modelsMu.Unlock()
		log.Printf("  model sync: feed free list is empty, keep cached list (no reconcile)")
		return 0, 0, nil
	}

	feed := make([]recommendedModelEntry, 0, len(payload.Free))
	for _, m := range payload.Free {
		feed = append(feed, recommendedModelEntry{ID: m.ID, Name: m.Name})
	}

	modelsMu.Lock()
	defer modelsMu.Unlock()
	added, removed = reconcileFeed(modelsCache, feed)
	modelsLastSync = time.Now()
	return added, removed, nil
}

const (
	modelFailNotFound = "not_found"
	modelFailPaid     = "paid"
	modelFailQuota    = "quota"
)

// classifyModelCallFailure 从上游错误响应中判定模型级失败类型。
// 纯函数，便于单测。返回空串表示与模型可用性无关（网络/账号/限流等）。
func classifyModelCallFailure(status int, body string) (kind, note string) {
	lower := strings.ToLower(body)
	switch {
	case status == 404 ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "unknown model") ||
		strings.Contains(lower, "invalid model") ||
		strings.Contains(lower, "promotion ended"):
		return modelFailNotFound, truncate(body, 200)
	case strings.Contains(lower, "subscribe") ||
		strings.Contains(lower, "subscription") ||
		strings.Contains(lower, "clinepass") ||
		strings.Contains(lower, "credit") ||
		strings.Contains(lower, "billing") ||
		strings.Contains(lower, "payment"):
		return modelFailPaid, truncate(body, 200)
	case strings.Contains(lower, "limit reached") ||
		strings.Contains(lower, "try again in"):
		return modelFailQuota, truncate(body, 200)
	}
	return "", ""
}

// noteModelCallFailure 把上游对某模型调用的失败结论写回模型缓存，
// 让列表反映真实可调用性：
//   - 404 / 促销结束：模型下架（removed），不再对客户端暴露；
//   - 上游要求订阅/扣费：cost 改标 quota（模型仍可路由，但不再承诺免费）；
//   - 每日免费额度到顶属于账号态，不改模型状态。
//
// 对未知模型 ID（不在缓存中）静默忽略。
func noteModelCallFailure(modelID string, status int, body string) {
	if strings.TrimSpace(modelID) == "" {
		return
	}
	kind, note := classifyModelCallFailure(status, body)
	if kind == "" || kind == modelFailQuota {
		return
	}
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	m, ok := modelsCache[modelID]
	if !ok {
		return
	}
	switch kind {
	case modelFailNotFound:
		if m.Status != ModelRemoved {
			m.Status = ModelRemoved
			m.Note = note
			m.SyncedAt = time.Now()
			log.Printf("  model %q marked removed: %s", modelID, note)
		}
	case modelFailPaid:
		if m.Cost != "quota" {
			m.Cost = "quota"
			m.Note = note
			log.Printf("  model %q re-tagged cost=quota: %s", modelID, note)
		}
	}
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

	added, removed, err := syncRecommendedModels()
	if err != nil {
		log.Printf("  model sync: failed (%v), using cached list", err)
		return
	}
	if added > 0 || removed > 0 {
		log.Printf("  model sync: feed reconcile: +%d added, -%d removed, %d usable", added, removed, len(getFreeModels()))
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
	// map 遍历无序，取字典序最小的可用模型，保证默认模型确定可复现。
	best := ""
	for id, m := range modelsCache {
		if m.Status != ModelActive {
			continue
		}
		if best == "" || id < best {
			best = id
		}
	}
	if best != "" {
		return best
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
	m, ok := modelsCache[id]
	status := ModelUnknown
	if ok {
		status = m.Status
	}
	modelsMu.Unlock()
	if ok && modelUsable(status) {
		return id
	}
	// 不在清单中的 ID 保持既有的封闭集语义；已下架的 ID 也不再透传，
	// 避免必然 404 的请求打到上游。
	if ok {
		log.Printf("  model %q is %q, fallback to %q", id, status, getDefaultModel())
	} else {
		log.Printf("  model %q not in free list, fallback to %q", id, getDefaultModel())
	}
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
			"id":                m.ID,
			"object":            "model",
			"created":           time.Now().UnixMilli(),
			"owned_by":          m.Provider,
			"source":            m.Source,
			"status":            m.Status,
			"cost":              m.Cost,
			"requiresStream":    m.RequiresStream,
			"context_window":    ctx,
			"contextWindow":     ctx,
			"max_output_tokens": maxOut,
			"maxOutputTokens":   maxOut,
			"syncedAt":          m.SyncedAt,
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
