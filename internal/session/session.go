// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（fast-path RLock / 双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
package session

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	stop    chan struct{}
}

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	r.stop = make(chan struct{})
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
}

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
// 命中且账号可用 → 滚动 lastActive 并直接返回；否则（lazy 异常情况）走重新分配。
func (r *Router) Resolve(key string) (string, bool) {
	now := time.Now()
	available := r.availableSet()

	// ── Fast path: RLock 快查 ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if available[e.uid] {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// 绑定号已冷却/占满 → 失效，落入慢路径重分配。
	}

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好。
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // 失效：清掉再分配
	}

	uids := r.availableSlice()
	if len(uids) == 0 {
		return "", false
	}

	// 双段策略：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	prev, existed := r.entries[key]
	r.entries[key] = entry{uid: uid, lastActive: now}
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// touch 滚动 lastActive 并异步镜像（只在快路径命中时写最后一次）。
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if found {
		r.cfg.Store.DelBind(key)
	}
	return found
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableSet 把 Available() 的有序列表转集合（快路径命中校验用）。
func (r *Router) availableSet() map[string]bool {
	uids := r.availableSlice()
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice 安全调用 Available（nil 函数视空池）。
func (r *Router) availableSlice() []string {
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey 从请求体提取会话键；按任务书给定顺序依次尝试，找不到返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. conversation_id
//  3. metadata.user_id
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["session_id"]); v != "" {
		return "session:" + v
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["user_id"]); v != "" {
			return v
		}
	}
	// Hermes Agent can expose its stable session identifier in the leading
	// system/developer prompt via --pass-session-id / HERMES_TUI_PASS_SESSION_ID=1.
	if messages, ok := obj["messages"].([]any); ok {
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role := strOrEmpty(msg["role"])
			if role != "system" && role != "developer" {
				break
			}
			if id := hermesSessionID(strOrEmpty(msg["content"])); id != "" {
				return "hermes:" + id
			}
		}
	}
	return ""
}

func hermesSessionID(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Session ID:") {
			continue
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "Session ID:"))
		if id != "" && !strings.ContainsAny(id, " \t") {
			return id
		}
	}
	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
