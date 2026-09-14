// Персистентность: запись/загрузка локального state.json на диск, зеркало снапшотов Redis (StoreSnapshotter),
// фоновый flusher, восстановление новейшего (RestoreFromSnapshot).
package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"workbuddy2api/internal/auth"
)

var flushInterval = 5 * time.Second

// persistLogEvery — напоминание каждую N-ю последовательную ошибку записи на диск (flusher тикает раз в 5s ≈ раз в минуту),
// чтобы не спамить в лог при постоянно полном диске/потере прав.
const persistLogEvery = 12

// snapshot — снапшот состояния пула (для зеркала Redis). Тот же источник, что у локального state.json (stateFile),
// плюс метка времени savedAt для «восстановления новейшего» (сравнение новизны локального и Redis-снапшота).
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// Pool — пул аккаунтов.
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// defaultIdle* — параметры компенсации простоя по умолчанию (ориентир claude-api selectWeightedRandom).
func (p *Pool) RestoreFromSnapshot() {
	store := p.store
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("[pool] источник восстановления=локальный state.json (нет снапшота Redis)")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// У снапшота нет savedAt: новизну сравнить нельзя, приоритет у локального.
		log.Printf("[pool] источник восстановления=локальный state.json (у снапшота Redis нет saved_at)")
		return
	}
	if localErr == nil && !localInfo.ModTime().After(snap.SavedAt) {
		// Снапшот не старше локального → берём снапшот.
		p.mu.Lock()
		p.applySnapshotLocked(snap)
		p.mu.Unlock()
		p.dirty.Store(true)
		log.Printf("[pool] источник восстановления=снапшот Redis (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	log.Printf("[pool] источник восстановления=локальный state.json (новее снапшота Redis %s)", snap.SavedAt.Format(time.RFC3339))
}

// Acquire занимает одно транзитное место для аккаунта; false означает, что аккаунт достиг лимита (или его нет).
// Вызывать после успешного Pick; вызывающий код отвечает за defer Release.
func (p *Pool) startFlusher() {
	interval := flushInterval // синхронно читаем до старта goroutine, чтобы не гонять с тестовой записью восстановления flushInterval
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			p.mu.Lock()
			if p.dirty.Swap(false) {
				p.saveLocked()
			}
			p.mu.Unlock()
		}
	}()
}

// Flush синхронно пишет состояние памяти на диск (идемпотентно: без изменений не пишет). Вызывать перед выходом процесса.
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) {
		p.saveLocked()
	}
	p.mu.Unlock()
}

// Add добавляет аккаунт; если уже есть — сохраняет состояние, обновляет учётные данные (upsert одного аккаунта, остальных не касается).
func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	p.applyAccountsLocked(sf.Accounts)
}

// applyAccountsLocked накрывает/вставляет byUID сохранённым состоянием аккаунтов (заглушечные учётные данные placeholder, Add заменит на полные).
// Общий для локального load() и восстановления из снапшота Redis; вызывающий код уже должен держать p.mu.
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	for uid, s := range accounts {
		// err_total в приоритете; err_count старого файла (последовательные ошибки) подмешиваем как разовый источник миграции (берём большее из двух,
		// максимально сохраняя исторический наблюдаемый сигнал — при старой семантике err_count тоже означал реально случавшиеся ошибки, терять нельзя).
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		p.byUID[uid] = &entry{
			a:            &auth.Auth{UID: uid}, // placeholder, Add заменит на полные учётные данные
			credits:      s.Credits,
			disabled:     s.Disabled,
			reason:       s.Reason,
			until:        s.Until,
			coolKind:     s.CoolKind,
			successCount: s.SuccessCount,
			errTotal:     errTotal,
			lastErr:      s.LastErr,
			lastSuccess:  s.LastSuccess,
			softStreak:   s.SoftStreak,
		}
	}
}

// applySnapshotLocked накрывает состояние памяти снапшотом Redis (вызывается после решения о новизне). Вызывающий код уже должен держать p.mu.
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(s.Accounts)
}
func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := p.stateOverviewLocked()
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		p.notePersistFail(err)
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.notePersistFail(err)
		return
	}
	if err := os.Rename(tmp, p.stateFp); err != nil {
		p.notePersistFail(err)
		return
	}
	if p.persistFails > 0 {
		// Восстановление после серии ошибок: одна строка в лог о восстановлении, чтобы «ошибки отгремели, а о выздоровлении никто не узнал».
		log.Printf("[pool] state.json снова сохраняется (до этого было %d последовательных ошибок)", p.persistFails)
		p.persistFails = 0
	}
	// Синхронно зеркалим один снапшот в Redis (fire-and-forget), сосуществует с локальным state.json как бэкап восстановления.
	if p.store != nil {
		snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
		if err == nil {
			p.store.SaveState(snapRaw)
		}
	}
}

// notePersistFail фиксирует одну ошибку записи локального state.json на диск и по правилу троттлинга решает, писать ли в лог:
// первая ошибка (переход успех→ошибка) — полная ошибка, каждое persistLogEvery-е последовательное — напоминание,
// остальные последовательные молчат (flusher тикает раз в 5s, при постоянно полном диске не спамим).
// Лог успешного восстановления пишет saveLocked на пути успеха. Выровнено с парадигмой трёх асинхронных записей redisstore
// «ошибка только в лог, наверх не пробрасываем», но ошибка записи на диск для эксплуатации — слепая зона, потому лишний слой троттлинга (notification).
func (p *Pool) notePersistFail(err error) {
	if p.persistFails == 0 {
		log.Printf("ERR: [pool] ошибка сохранения state.json: %v", err)
	} else if p.persistFails%persistLogEvery == 0 {
		log.Printf("ERR: [pool] state.json не сохраняется %d раз подряд: %v", p.persistFails, err)
	}
	p.persistFails++
}

// stateOverviewLocked собирает текущее состояние памяти в stateFile (общее для записи на диск + зеркала снапшотов). Вызывающий код уже должен держать p.mu.
func (p *Pool) stateOverviewLocked() stateFile {
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = stateAccount{
			Credits:      e.credits,
			Disabled:     e.disabled,
			Reason:       e.reason,
			Until:        e.until,
			CoolKind:     e.coolKind,
			SuccessCount: e.successCount,
			ErrTotal:     e.errTotal,
			LastSuccess:  e.lastSuccess,
			LastErr:      e.lastErr,
			SoftStreak:   e.softStreak,
		}
	}
	return sf
}
