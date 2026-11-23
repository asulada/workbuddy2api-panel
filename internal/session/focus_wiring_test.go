package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/redisstore"
)

// focusRouter 构造一个「校验集全量、新会话分配池收窄到 target」的路由器，
// 模拟 pool.expiring_focus 生效时的形态。
func focusRouter(store redisstore.Store, all []string, target string, ttl time.Duration) *Router {
	return New(Config{
		TTL:                ttl,
		Store:              store,
		Available:          func() []string { return all },
		AvailableForModel:  func(string) []string { return all },
		NewSessionForModel: func(string) []string { return []string{target} },
	})
}

// TestNewSessionGoesToFocusTarget 新会话（无绑定）必须分到聚焦目标号。
func TestNewSessionGoesToFocusTarget(t *testing.T) {
	r := focusRouter(newCountingStore(), []string{"a", "b", "c"}, "b", time.Minute)
	for i := 0; i < 50; i++ {
		uid, ok := r.ResolveForModel(fmt.Sprintf("new-%d", i), "")
		if !ok || uid != "b" {
			t.Fatalf("新会话 %d 应分到目标号 b，实际 %q ok=%v", i, uid, ok)
		}
	}
}

// TestBoundSessionKeepsOriginalAccount 已绑定的老会话**不受新会话分配池影响**：
// 校验集是全量，故快路径命中后继续用原号，不迁移。
//
// 这是「老会话保留原号，靠 TTL 自然过期再迁移」的核心断言——若把校验集也收窄，
// 老会话会立刻被判失效并全量迁移，切换瞬间所有前缀缓存 miss。
func TestBoundSessionKeepsOriginalAccount(t *testing.T) {
	r := focusRouter(newCountingStore(), []string{"a", "b", "c"}, "b", time.Minute)
	// 模拟「聚焦开启前就已存在的两个会话」。
	r.Bind("old-1", "a")
	r.Bind("old-3", "c")

	for i := 0; i < 20; i++ {
		if uid, ok := r.ResolveForModel("old-1", ""); !ok || uid != "a" {
			t.Fatalf("老会话应继续用原号 a，实际 %q ok=%v", uid, ok)
		}
		if uid, ok := r.ResolveForModel("old-3", ""); !ok || uid != "c" {
			t.Fatalf("老会话应继续用原号 c，实际 %q ok=%v", uid, ok)
		}
	}
}

// TestBoundSessionMigratesAfterTTL 老会话在 TTL 过期后按当时的分配池重新指派，
// 即迁到聚焦目标号——「渐进迁移」的另一半。
func TestBoundSessionMigratesAfterTTL(t *testing.T) {
	r := focusRouter(newCountingStore(), []string{"a", "b", "c"}, "b", 10*time.Millisecond)
	r.Bind("old", "a")

	// TTL 内：原号。
	if uid, _ := r.ResolveForModel("old", ""); uid != "a" {
		t.Fatalf("TTL 内应用原号 a，实际 %q", uid)
	}
	time.Sleep(20 * time.Millisecond)
	// 过期后重新分配 → 新会话分配池只有 b。
	if uid, ok := r.ResolveForModel("old", ""); !ok || uid != "b" {
		t.Fatalf("TTL 过期后应迁到目标号 b，实际 %q ok=%v", uid, ok)
	}
}

// TestNewSessionFallsBackWhenNoNewSessionFunc 未注入 NewSessionForModel 时，
// 分配行为与引入该字段前完全一致（零回归的硬约束）。
func TestNewSessionFallsBackWhenNoNewSessionFunc(t *testing.T) {
	st := newCountingStore()
	avail := []string{"x", "y"}
	r := New(Config{
		TTL:               time.Minute,
		Store:             st,
		Available:         func() []string { return avail },
		AvailableForModel: func(string) []string { return avail },
	})
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		uid, ok := r.ResolveForModel(fmt.Sprintf("s-%d", i), "")
		if !ok {
			t.Fatal("should resolve")
		}
		seen[uid] = true
		if uid != "x" && uid != "y" {
			t.Fatalf("未注入新会话分配池时应只在 AvailableForModel 里选，实际 %q", uid)
		}
	}
	if !seen["x"] || !seen["y"] {
		t.Fatalf("未注入时应照常散列到两个号，实际 %v", seen)
	}
}
