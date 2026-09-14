package server

import (
	"sync"
	"time"
)

// degradeGate — автомат деградации: если в режиме passthrough запрос отклонён контент-политикой апстрима,
// переключаемся на нейтральный промпт Degraded до сброса в ближайшие 00:00 CST.
//
// Назначение: обработка ложных срабатываний (та же философия, что в internal/upstream/sanitize.go) — блокировки
// контента чаще всего вызваны ложным срабатыванием отпечатка из system, обходятся минимальным нейтральным промптом; не является каркасом обхода.
//
// Автомат: состояние в памяти процесса, сбрасывается при рестарте (приемлемо: рестарты редки, а режим custom
// вообще не входит в путь деградации). Запросы в период деградации идут сразу на Degraded, без первого 400.
type degradeGate struct {
	mu    sync.Mutex
	until time.Time
}

// Active — находимся ли сейчас в периоде деградации (now < until).
func (g *degradeGate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Trigger запускает деградацию до ближайших 00:00 CST (Asia/Shanghai).
// Если период уже активен — не продлеваем (сохраняется семантика сброса в 00:00 от первого срабатывания).
func (g *degradeGate) Trigger() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !time.Now().Before(g.until) {
		// Уже истёк/не запускался → выставляем новый until; иначе сохраняем прежний until (без продления).
		g.until = nextMidnightCST(time.Now())
	}
}

// nextMidnightCST возвращает ближайшие после now 00:00 Asia/Shanghai (чистая функция, удобна для тестов).
//
// Граничная семантика:
//   - 23:59 → 00:00 следующего дня (через несколько секунд)
//   - 00:00 → 00:00 следующего дня (сразу после полуночи следующая полночь — завтра)
//
// Считаем с фиксированным смещением +08:00, чтобы не зависеть от часового пояса системы (в контейнере/на хосте он непредсказуем).
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// Переводим now на взгляд CST, берём 00:00 текущего дня и прибавляем сутки; если эти 00:00 <= now — прибавляем ещё сутки.
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
