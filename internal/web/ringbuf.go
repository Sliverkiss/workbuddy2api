// ringbuf.go 定容环形缓冲：请求行 / stdout 行 / 任务事件的内存留存。
package web

import "sync"

// ring[T] 定容环形缓冲：满后覆盖最旧；读出走序（最旧→最新）。
type ring[T any] struct {
	mu   sync.RWMutex
	buf  []T
	head int // 下一个写入位
	len  int
}

func newRing[T any](cap int) *ring[T] {
	if cap <= 0 {
		cap = 100
	}
	return &ring[T]{buf: make([]T, cap)}
}

func (r *ring[T]) add(v T) {
	r.mu.Lock()
	r.buf[r.head] = v
	r.head = (r.head + 1) % len(r.buf)
	if r.len < len(r.buf) {
		r.len++
	}
	r.mu.Unlock()
}

// list 返回最旧→最新的全部元素；limit>0 时只取最后 limit 个。
func (r *ring[T]) list(limit int) []T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.len
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]T, 0, n)
	for i := r.len - n; i < r.len; i++ {
		out = append(out, r.buf[(r.head+len(r.buf)-r.len+i)%len(r.buf)])
	}
	return out
}

// listFiltered 按 keep 过滤（walk 方向最旧→最新），limit>0 取最后 limit 个命中。
func (r *ring[T]) listFiltered(limit int, keep func(T) bool) []T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []T
	for i := 0; i < r.len; i++ {
		v := r.buf[(r.head+len(r.buf)-r.len+i)%len(r.buf)]
		if keep(v) {
			out = append(out, v)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}
