package main

import (
	"testing"
	"time"
)

// resetModelsCacheForTest 清空模型缓存，让每个测试从确定的初始状态开始。
func resetModelsCacheForTest(t *testing.T) {
	t.Helper()
	modelsMu.Lock()
	modelsCache = nil
	modelsLastSync = time.Time{}
	modelsMu.Unlock()
	initModelsCache() // 重新注入 seed 兜底
}

func TestReconcileFeedAddsAndRetires(t *testing.T) {
	resetModelsCacheForTest(t)

	modelsMu.Lock()
	before := len(modelsCache)
	added, removed := reconcileFeed(modelsCache, []recommendedModelEntry{
		{ID: "cline-free/mimo-v2.6-flash", Name: "Mimo V2.6 Flash"},
	})
	after := make(map[string]ModelStatus, len(modelsCache))
	for id, m := range modelsCache {
		after[id] = m.Status
	}
	modelsMu.Unlock()

	if before != 3 {
		t.Fatalf("expected 3 seed models, got %d", before)
	}
	if added != 1 || removed != 3 {
		t.Fatalf("expected added=1 removed=3, got added=%d removed=%d", added, removed)
	}
	if after["cline-free/mimo-v2.6-flash"] != ModelActive {
		t.Fatalf("feed model should be active, got %q", after["cline-free/mimo-v2.6-flash"])
	}
	for _, id := range []string{
		"deepseek/deepseek-v4-flash",
		"poolside/laguna-s-2.1:free",
		"stepfun/step-3.7-flash",
	} {
		if after[id] != ModelRemoved {
			t.Fatalf("seed %q must be retired by reconcile, got %q", id, after[id])
		}
	}
}

func TestReconcileFeedReactivatesRemoved(t *testing.T) {
	resetModelsCacheForTest(t)

	modelsMu.Lock()
	modelsCache["deepseek/deepseek-v4-flash"].Status = ModelRemoved
	added, removed := reconcileFeed(modelsCache, []recommendedModelEntry{
		{ID: "deepseek/deepseek-v4-flash"},
	})
	modelsMu.Unlock()

	// deepseek 被 feed 重新激活；另两个 seed 不在 feed 中，按对账语义下架。
	if added != 0 || removed != 2 {
		t.Fatalf("expected added=0 removed=2, got added=%d removed=%d", added, removed)
	}
	modelsMu.Lock()
	st := modelsCache["deepseek/deepseek-v4-flash"].Status
	modelsMu.Unlock()
	if st != ModelActive {
		t.Fatalf("feed reappearance must reactivate model, got %q", st)
	}
}

func TestGetFreeModelsExcludesRemoved(t *testing.T) {
	resetModelsCacheForTest(t)

	modelsMu.Lock()
	modelsCache["stepfun/step-3.7-flash"].Status = ModelRemoved
	modelsMu.Unlock()

	free := getFreeModels()
	ids := map[string]bool{}
	for _, m := range free {
		ids[m.ID] = true
	}
	if ids["stepfun/step-3.7-flash"] {
		t.Fatal("removed model must not appear in client-facing list")
	}
	all := getAllModels()
	if len(all) <= len(free) {
		t.Fatalf("admin view must include removed models: all=%d free=%d", len(all), len(free))
	}
}

func TestGetDefaultModelDeterministic(t *testing.T) {
	resetModelsCacheForTest(t)

	modelsMu.Lock()
	modelsCache["deepseek/deepseek-v4-flash"].Status = ModelRemoved
	modelsCache["zzz/fallback"] = &ModelInfo{ID: "zzz/fallback", Status: ModelActive}
	modelsCache["aaa/first"] = &ModelInfo{ID: "aaa/first", Status: ModelActive}
	modelsMu.Unlock()

	got := getDefaultModel()
	if got != "aaa/first" {
		t.Fatalf("expected deterministic smallest active id, got %q", got)
	}
}

func TestNormalizeRequestModelFallsBackForRemoved(t *testing.T) {
	resetModelsCacheForTest(t)

	modelsMu.Lock()
	modelsCache["deepseek/deepseek-v4-flash"].Status = ModelRemoved
	modelsCache["aaa/first"] = &ModelInfo{ID: "aaa/first", Status: ModelActive}
	modelsMu.Unlock()

	if got := normalizeRequestModel("deepseek/deepseek-v4-flash"); got != "aaa/first" {
		t.Fatalf("removed model must fall back to default, got %q", got)
	}
	if got := normalizeRequestModel("aaa/first"); got != "aaa/first" {
		t.Fatalf("active model must be honored, got %q", got)
	}
}

func TestClassifyModelCallFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"404", 404, `{"error":"model not found","success":false}`, modelFailNotFound},
		{"promotion ended", 403, "Free model promotion ended. The free promotion for this model has ended.", modelFailNotFound},
		{"pass subscription", 403, "No access to ClinePass subscription models yet. Subscribe to ClinePass...", modelFailPaid},
		{"credits", 402, `{"error":"insufficient credits"}`, modelFailPaid},
		{"quota limit", 429, `{"error":"Daily free model limit reached, try again in 17h 59m"}`, modelFailQuota},
		{"unrelated", 500, `<html>internal error</html>`, ""},
	}
	for _, c := range cases {
		kind, _ := classifyModelCallFailure(c.status, c.body)
		if kind != c.want {
			t.Fatalf("%s: expected %q got %q", c.name, c.want, kind)
		}
	}
}

func TestNoteModelCallFailure(t *testing.T) {
	t.Run("404 marks removed", func(t *testing.T) {
		resetModelsCacheForTest(t)
		noteModelCallFailure("deepseek/deepseek-v4-flash", 404, `{"error":"model not found","success":false}`)
		modelsMu.Lock()
		m := modelsCache["deepseek/deepseek-v4-flash"]
		modelsMu.Unlock()
		if m == nil || m.Status != ModelRemoved {
			t.Fatalf("expected removed, got %+v", m)
		}
		if m.Note == "" {
			t.Fatal("expected failure note to be recorded")
		}
	})

	t.Run("billing retags cost", func(t *testing.T) {
		resetModelsCacheForTest(t)
		noteModelCallFailure("deepseek/deepseek-v4-flash", 403, "No access to ClinePass subscription models yet.")
		modelsMu.Lock()
		m := modelsCache["deepseek/deepseek-v4-flash"]
		modelsMu.Unlock()
		if m == nil {
			t.Fatal("model missing from cache")
		}
		if m.Status != ModelActive {
			t.Fatalf("billing error must not remove a callable model, got %q", m.Status)
		}
		if m.Cost != "quota" {
			t.Fatalf("expected cost=quota, got %q", m.Cost)
		}
	})

	t.Run("quota does not change model state", func(t *testing.T) {
		resetModelsCacheForTest(t)
		noteModelCallFailure("deepseek/deepseek-v4-flash", 429, `{"error":"Daily free model limit reached, try again in 17h 59m"}`)
		modelsMu.Lock()
		m := modelsCache["deepseek/deepseek-v4-flash"]
		modelsMu.Unlock()
		if m.Status != ModelActive || m.Cost != "free" {
			t.Fatalf("quota error must not change model state, got status=%q cost=%q", m.Status, m.Cost)
		}
	})

	t.Run("unknown model id ignored", func(t *testing.T) {
		resetModelsCacheForTest(t)
		before := len(modelsCache)
		noteModelCallFailure("never/heard-of", 404, "model not found")
		modelsMu.Lock()
		_, exists := modelsCache["never/heard-of"]
		size := len(modelsCache)
		modelsMu.Unlock()
		if exists || size != before {
			t.Fatal("failure feedback must not create cache entries")
		}
	})
}
