package upstream

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestMonitorBodyIdleCutoff 静默超过 idle（配小值）→ Read 返回错误（context canceled）。
func TestMonitorBodyIdleCutoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 模拟真实 resp.Body：阻塞直到请求 context 被 cancel 才返回 error。
	body := monitorBody(&ctxBoundReader{ctx: ctx}, 50*time.Millisecond, cancel)
	defer body.Close()
	if _, err := body.Read(make([]byte, 16)); err == nil {
		t.Fatal("expect read error after idle cutoff")
	}
}

// TestMonitorBodyRenewsOnActivity 持续活跃（周期吐数据，总时长 > idle）→ 流不被掐。
func TestMonitorBodyRenewsOnActivity(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 每 30ms 吐 1 字节；idle=80ms。总读 10 字节耗时 300ms > idle：
	// 若空闲续命失效（绝对 deadline 语义）早在 80ms 就被掐断。
	body := monitorBody(nopCloserBody{Reader: periodicReader{period: 30 * time.Millisecond}}, 80*time.Millisecond, cancel)
	defer body.Close()
	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		if _, err := io.ReadFull(body, buf); err != nil {
			t.Fatalf("read %d: %v (active stream must not be cut)", i, err)
		}
	}
}

// TestMonitorBodyDisabledWhenIdleZero idle<=0 直接返回原底流。
func TestMonitorBodyDisabledWhenIdleZero(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := nopCloserBody{Reader: &oneByteReader{}}
	out := monitorBody(rc, 0, cancel)
	if _, ok := out.(nopCloserBody); !ok {
		t.Fatalf("idle<=0 should return underlying body verbatim, got %T", out)
	}
	buf := make([]byte, 1)
	if _, err := out.Read(buf); err != nil || buf[0] != 'a' {
		t.Fatalf("read=%q err=%v", buf, err)
	}
}

// TestMonitorBodyCloseStopsGoroutine Close 停掉 goroutine，无泄漏（等效监控循环 + 短周期）。
func TestMonitorBodyCloseStopsGoroutine(t *testing.T) {
	m := &idleMonitoringBody{
		rc:       &ctxBoundReader{ctx: context.Background()},
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   func() {},
	}
	stopped := make(chan struct{})
	go func() {
		tk := time.NewTicker(idleTick(40 * time.Millisecond))
		defer tk.Stop()
		for {
			select {
			case <-m.stopCh:
				close(stopped)
				return
			case <-tk.C:
			}
		}
	}()
	m.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("goroutine not stopped after Close")
	}
}

// —— helpers ——

// ctxBoundReader 模拟 net/http resp.Body：Read 阻塞在 ctx.Done() 上，ctx cancel 后返回错误。
type ctxBoundReader struct{ ctx context.Context }

func (r *ctxBoundReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *ctxBoundReader) Close() error { return nil }

type periodicReader struct{ period time.Duration }

func (r periodicReader) Read(p []byte) (int, error) {
	time.Sleep(r.period)
	p[0] = 'x'
	return 1, nil
}

type oneByteReader struct{ done bool }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 'a'
	return 1, nil
}

type nopCloserBody struct{ io.Reader }

func (nopCloserBody) Close() error { return nil }
