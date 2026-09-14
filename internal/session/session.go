// Package session — липкая маршрутизация сессий: одна сессия (ключ conversationId / metadata) по возможности сидит на одном номере.
//
// Дизайн сверялись с antigravityProxyGo internal/session (fast-path RLock / двухсегментное распределение / TTL / персистентность),
// но переделали на чистую память + асинхронное зеркало в redisstore:
//   - попадание идёт быстрым поиском под RLock (подавляющее большинство запросов уже привязано);
//   - промах/протухание идёт распределением после re-check под замком записи, чтобы не раздать один key дважды из конкурентности (защита от TOCTOU);
//   - распределение сначала хеширует «свободные номера» (доступные, не привязанные ни к одной сессии), затем весь пул (двухсегментная стратегия);
//   - LastActive катится продлением, протухшие по TTL чистят фоновый GC или ленивое протухание быстрого пути;
//   - каждое изменение привязки зеркалируем в redisstore по fire-and-forget (чтобы рестарт не ронял липкость).
package session

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/redisstore"
)

// entry — одна привязка сессии.
type entry struct {
	uid        string
	lastActive time.Time
}

// Config — зависимости роутера; Available возвращает упорядоченный список uid «доступных номеров» (healthy и с незаполненным in-flight),
// предоставляет pool.AvailableUIDs. Store может быть redisstore.Noop (чистая память).
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
}

// Router — роутер липких сессий.
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	stop    chan struct{}
}

// New создаёт роутер. Если cfg.Store равен nil — берём Noop (чистая память); cfg.Available равный nil считаем пустым пулом.
// Неположительные TTL/GCInterval заменяем умолчаниями (30m / 5m) — main передаёт распарсенное из config, здесь страховка.
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

// StartGC запускает фоновую GC-goroutine (идемпотентно). При выходе процесса вызываем StopGC.
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

// StopGC останавливает фоновую GC (идемпотентно).
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore при старте восстанавливает привязки из redisstore (память поверх локального, чтение происходит только здесь).
// Уже существующие локальные привязки сохраняем — Redis лишь бэкап для восстановления, локальное после создания авторитетно.
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
	log.Printf("[session] восстановлено %d липких привязок сессий из Redis", loaded)
	}
}

// Resolve возвращает uid номера, к которому должен липнуть ключ сессии; ok=false означает, что доступных номеров сейчас нет.
// Попали и номер доступен → катим lastActive и сразу возвращаем; иначе (ленивый исключительный случай) идём перераспределять.
func (r *Router) Resolve(key string) (string, bool) {
	now := time.Now()
	available := r.availableSet()

// ── Fast path: быстрый поиск под RLock ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if available[e.uid] {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// Привязанный номер охлаждён/занят → привязка недействительна, падаем в медленный путь перераспределения.
	}

// ── Slow path: распределение после re-check под замком записи ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check: конкурентный тот же key другая горутина уже могла распределить.
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // недействительна: стираем и распределяем заново
	}

	uids := r.availableSlice()
	if len(uids) == 0 {
		return "", false
	}

	// Двухсегментная стратегия: сначала «свободные номера» (доступные, не привязанные ни к одной сессии), затем весь пул.
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

// touch катит lastActive и зеркалирует асинхронно (пишем напоследок только при попадании быстрого пути).
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Bind явно привязывает ключ сессии к uid (идемпотентно поверх старого) и зеркалирует асинхронно в redisstore.
// Для «липкость следует за финально успешным номером»: перед успешным ответом перепривязываем сессию на реально успешный номер, чтобы следующий ход многодиалога стабильно
// сходился к «номеру, стабильно успешному для этой сессии» (вровень с семантикой antigravity). Пустой key — сразу возврат (без сессии не привязываем).
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

// Unbind снимает привязку сессии (вызываем при неудаче запроса, чтобы сессия в следующий раз перераспределилась). Возвращает, была ли.
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

// Count возвращает текущее число привязок (для наблюдения через /status).
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce чистит протухшие по TTL привязки и зеркалирует удаление.
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

// availableSet превращает упорядоченный список Available() во множество (для проверки попадания быстрого пути).
func (r *Router) availableSet() map[string]bool {
	uids := r.availableSlice()
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice безопасно вызывает Available (nil-функцию считаем пустым пулом).
func (r *Router) availableSlice() []string {
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex — взятие хеша FNV-1a по модулю (стабильный хеш двухсегментного распределения antigravity).
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey извлекает ключ сессии из тела запроса; пробуем по порядку ниже, не нашли — возвращаем пустую строку (никогда не ошибаемся).
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. metadata.user_id
//
// issue #35: клиент реально шлёт camelCase conversationId, раньше распознавали только snake_case,
// из-за чего липкий роутер мазал, один диалог крутился по разным номерам и контекстный кэш апстрима промахивался. Теперь распознаём оба написания,
// приоритет snake_case выше camelCase (при одном значении под разными именами одного диалога возвращается то же значение, смешения нет по построению).
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
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["user_id"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	return strOrEmpty(obj["conversationId"])
}

// strOrEmpty безопасно превращает строковое поле JSON в string (нестроковый тип возвращает пусто).
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
