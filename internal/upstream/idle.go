// idle.go — контроль простоя в chat SSE-потоке: пока данные идут, соединение живёт; рвём только при тишине дольше порога (освобождая аренду).
package upstream

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleMonitoringBody оборачивает chat SSE-body:
// каждое чтение данных из нижележащего слоя (n>0) обновляет lastRead; фоновая goroutine периодически проверяет,
// при тишине дольше idle отменяет context запроса, прерывая заблокированный Read.
type idleMonitoringBody struct {
	rc       io.ReadCloser
	mu       sync.Mutex
	lastRead time.Time
	stopOnce sync.Once
	stopCh   chan struct{}
	cancel   context.CancelFunc
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

// Close останавливает фоновую goroutine, отменяет context запроса и закрывает нижележащий поток, исключая утечки.
func (b *idleMonitoringBody) Close() error {
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.cancel()
	return b.rc.Close()
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// monitorBody при idle<=0 возвращает исходный поток как есть (контроль простоя отключён);
// иначе оборачивает контролем простоя в потоке. Отсчёт начинается после возврата body — фазу первого байта ведёт
// Transport.ResponseHeaderTimeout, здесь не забегаем вперёд.
func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	b := &idleMonitoringBody{
		rc:       rc,
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   cancel,
	}
	go func() {
		t := time.NewTicker(idleTick(idle))
		defer t.Stop()
		for {
			select {
			case <-b.stopCh:
				return
			case <-t.C:
				if b.idleFor() > idle {
					cancel()
					return
				}
			}
		}
	}()
	return b
}

// idleTick возвращает период проверки: idle/4, зажат в [10ms, 1s]. Малые idle обнаруживаются быстро, большие не крутятся вхолостую.
func idleTick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
