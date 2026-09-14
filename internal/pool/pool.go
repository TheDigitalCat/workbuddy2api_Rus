// Pool — ядро пула аккаунтов: определение структуры, конструирование (инъекции New/Set*), транзитные аренды (Acquire/Release)
// и добавление/удаление аккаунтов (Add/SyncToDir/upsertLocked). Выбор номера/кулдаун/состояние/персистентность — в других файлах пакета.
package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // в памяти есть изменения, ждущие записи на диск
	// store — зеркало снапшотов состояния пула (redisstore.Store); nil = без зеркала (Redis не настроен / вне Noop тоже может быть nil).
	// SaveState/LoadState подключены через него, сосуществует с локальным state.json как бэкап для восстановления при старте.
	store StoreSnapshotter
	// Настройка прерывателя (инъекция SetBreaker; значения по умолчанию — см. defaultBreaker*).
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax — потолок экспоненциального отката мягкого кулдауна (инъекция SetSoftRateMax; по умолчанию defaultSoftRateMax).
	softRateMax time.Duration
	// Настройка трёхфакторного взвешивания (инъекция SetWeights; значения по умолчанию — см. defaultIdle*).
	idleWeightPerHour float64
	idleWeightMax     float64
	// maxInFlight — максимум транзитных запросов на аккаунт; 0 = без ограничений (аренда выключена).
	maxInFlight int
	// randInt64N — только для инъекции детерминированного источника случайности в тестах; при nil используется глобальный источник math/rand/v2.
	// Производственный код это поле задавать не должен.
	randInt64N func(n int64) int64
	// persistFails — счётчик последовательных ошибок записи локального state.json на диск (читается/пишется только saveLocked под замком, atomic не нужен).
	// Нужен для троттлинга логов ошибок записи: первая ошибка/каждое N-е напоминание/восстановление — по одной строке, чтобы не спамить при полном диске.
	persistFails int
}

// defaultBreaker* — параметры прерывателя по умолчанию (ориентир FreeBuff2API).
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetBreaker внедряет параметры прерывателя (main вызывает после разбора config). Неположительные значения сохраняют текущие (по умолчанию).
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax внедряет потолок экспоненциального отката мягкого кулдауна (main вызывает после разбора config).
// Неположительные значения сохраняют текущее (по умолчанию 2h), стиль как у SetBreaker.
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetWeights внедряет параметры компенсации простоя для трёхфакторного взвешивания. Неположительные значения сохраняют текущие (по умолчанию).
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetMaxInFlight внедряет максимум транзитных запросов на аккаунт; 0 = без ограничений. Отрицательные значения сохраняют текущее.
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetStore внедряет зеркало снапшотов состояния пула (redisstore.Store). nil означает без зеркала (только локальное восстановление).
// Вызывать до SyncToDir, чтобы «восстановление новейшего» происходило до сверки аккаунтов.
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot — восстановление новейшего: сравнивает локальный state.json и снапшот Redis, берёт более новый.
// Нет снапшота, у снапшота нет savedAt, локальный файл отсутствует/нечитаем — всё это считается «приоритет локального/пропуск снапшота»,
// плюс одна строка в лог об источнике восстановления. Вызывать до SyncToDir (SyncToDir только добавляет/удаляет, значений не вносит).
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	limit := p.maxInFlight
	p.mu.RUnlock()
	if !ok {
		return false
	}
	if limit <= 0 {
		// Без ограничений: счётчик всё равно растёт (для наблюдения за состоянием), но отказов никогда нет.
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release освобождает одно транзитное место. Идемпотентно уменьшает до 0 (защита от двойного освобождения в минус).
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource — только для инъекции детерминированного источника случайности в тестах; производственный код вызывать не должен.
// После инъекции источника, возвращающего n∈[0,n), результат жеребьёвки pickWeighted полностью предсказуем.
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// startFlusher каждые flushInterval проверяет флаг dirty, при изменениях пишет на диск через saveLocked.
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir сверяет пул с последним результатом сканирования: новые аккаунты добавляются, исчезнувшие удаляются (состояние сохраняется).
// Результат удаления сохраняется обратно в state.json, чтобы удалённые аккаунты не воскресали через load() при следующем старте.
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// upsertLocked обновляет или вставляет один аккаунт; если уже есть — меняет только учётные данные, сохраняя состояние credits/cooling.
// Вызывающий код уже должен держать p.mu; Add и SyncToDir используют эту общую логику upsert.
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // сохраняем состояние credits/cooling
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick возвращает healthy-аккаунт с наибольшим балансом; если доступных нет — nil.
