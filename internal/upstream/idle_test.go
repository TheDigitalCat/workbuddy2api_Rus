package upstream

import (
	"context"
	"io"
	"testing"
	"time"
)

// TestMonitorBodyIdleCutoff — тишина дольше idle (ставим малое) → Read возвращает ошибку (context canceled).
func TestMonitorBodyIdleCutoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Эмулируем реальный resp.Body: блокируемся, пока context запроса не отменят, затем возвращаем error.
	body := monitorBody(&ctxBoundReader{ctx: ctx}, 50*time.Millisecond, cancel)
	defer body.Close()
	if _, err := body.Read(make([]byte, 16)); err == nil {
		t.Fatal("expect read error after idle cutoff")
	}
}

// TestMonitorBodyRenewsOnActivity — постоянная активность (периодически отдаём данные, суммарно дольше idle) → поток не рвём.
func TestMonitorBodyRenewsOnActivity(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Каждые 30ms отдаём 1 байт; idle=80ms. Чтение 10 байт занимает 300ms > idle:
	// при сломанном продлении простоя (семантика абсолютного deadline) поток оборвали бы уже на 80ms.
	body := monitorBody(nopCloserBody{Reader: periodicReader{period: 30 * time.Millisecond}}, 80*time.Millisecond, cancel)
	defer body.Close()
	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		if _, err := io.ReadFull(body, buf); err != nil {
			t.Fatalf("read %d: %v (active stream must not be cut)", i, err)
		}
	}
}

// TestMonitorBodyDisabledWhenIdleZero — при idle<=0 возвращаем исходный поток как есть.
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

// TestMonitorBodyCloseStopsGoroutine — Close останавливает goroutine, без утечек (эквивалент цикла мониторинга + короткий период).
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

// ctxBoundReader эмулирует net/http resp.Body: Read блокируется на ctx.Done(), после отмены ctx возвращает ошибку.
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
