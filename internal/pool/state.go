// Эволюция и опрос состояния аккаунта: отключение/подсчёт 12153 подряд, учёт успехов и ошибок, разморозка-возрождение,
// а также опросы состояния (Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List).
package pool

import (
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
		p.dirty.Store(true)
	}
}

// NoteSessionDead фиксирует один ErrSessionDead (12153) — **без немедленного отключения**.
// Старое поведение отключало (Disable) за один 12153, но 12153 срабатывает эпизодически (сетевой джиттер/мигание апстрима/гонка
// refresh), и вечное убийство номера за одну ошибку косило здоровые аккаунты (разведка P0-1: все 13 disabled-номеров успешно refresh,
// жертвы исторического ошибочного решения). Теперь отключаем только после sessionDeadThreshold подряд:
// счёт +1, достиг порога → Disable (reason=12153 session dead) и сброс счёта;
// успех refresh / любой успех / ручное возрождение → ClearSessionDead сбрасывает счёт.
// Возвращает true, если порог достигнут и отключение выполнено в этом вызове.
// Даже если аккаунт уже disabled, счёт всё равно растёт и первые N-1 раз возвращается false — но keepalive пропускает
// disabled-номера, так что сюда реально попадают лишь сценарии вроде «disabled возродили, а счёт не сброшен».
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		return false
	}
	e.disabled = true
	e.reason = sessionDeadReason
	e.sessionDeadFails = 0
	p.dirty.Store(true)
	return true
}

// ClearSessionDead сбрасывает счёт последовательных 12153 — вызывать в любой момент доказанной живости аккаунта:
// успех refresh (RunKeepaliveNow), успех chat (NoteSuccess), ручное возрождение (ReviveDisabled).
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.sessionDeadFails = 0
	}
}

// ReviveDisabled — вход ручного/конечного возрождения: снимает disabled + reason + счёт последовательных 12153,
// аккаунт возвращается в пул (если нет других кулдаунов/прерываний — сразу выбираем, дальше подхватывает healthcheck).
// **Не меняет** существующую семантику Disabled в выборе номера/эндпоинтах состояния: disabled-номер по-прежнему не участвует в выборе,
// пока не возрождён этим методом. Несуществующий uid — пустая операция.
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.disabled {
		e.disabled = false
		e.reason = ""
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// reviveCoolingLocked снимает только кулдаун (until/coolKind/reason/softStreak) и обновляет credits, прерыватель не трогает
// (fails/retryCount/breakerUntil). Разморозка чекином идёт сюда: успех чекина доказывает лишь восстановление баланса и
// здоровье billing-канала, но не chat-канала, поэтому прерывание (серия сигналов 5xx) чекин перекрывать не должен.
// softStreak относится к **домену кулдауна** (тот же домен, что until/coolKind), потому обнуляется вместе с кулдауном — согласно существующей семантике C5 «разморозка снимает только кулдаун,
// не прерывание»; жёсткий кулдаун (CoolHard) в streak и так не участвует, здесь снимается историческое накопление мягких кулдаунов.
// Вызывающий код уже должен держать p.mu.
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
			p.reviveCoolingLocked(e, remain)
		} else {
			e.credits = remain
		}
		p.dirty.Store(true)
	}
}

// NoteError фиксирует одну ошибку: кормит единственный счётчик последовательных ошибок fails + накопленные ошибки errTotal.
// По достижении breakerThreshold срабатывает прерыватель (экспоненциальный откат), вся семантика последовательных ошибок merged в прерыватель (отдельного err-кулдауна больше нет).
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteSuccess: успешный запрос наращивает счёт успехов, обновляет lastSuccess и расчищает последовательные ошибки и рабочее состояние прерывателя.
// Бинарная модель: снимаем fails + retryCount + breakerUntil; until/coolKind не трогаем (это мгновенные кулдауны, истекают сами).
// Дополнительно снимаем softStreak: успех — сильнейшее доказательство восстановления аккаунта, счёт последовательных мягких лимитов обнуляется, откат возвращается к базе.
// Так же снимаем sessionDeadFails: успех доказывает, что сессия жива (семантика как у ClearSessionDead).
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// Status опрашивает состояние одного аккаунта.
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID возвращает полные учётные данные аккаунта (для планировщика/эксплуатационных интерфейсов).
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs возвращает список UID текущих healthy-аккаунтов со свободными транзитными местами (сортировка по UID, стабильный вывод).
// Нужно sticky-маршрутизации сессий (internal/session) для быстрой проверки попадания + двухсегментного распределения; если доступных нет — пустой срез.
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUID: если uid сейчас healthy и транзитные места не заняты, возвращает его учётные данные (пишет lastUsed против столкновений);
// иначе возвращает nil. Нужно sticky-маршрутизации сессий для проверки попадания и прямого взятия.
func (p *Pool) PickByUID(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// CountsDetailed возвращает пять счётчиков: total/healthy/cooling/disabled/inFlightFull.
// cooling включает обычные кулдауны (until) и периоды прерывания (breakerUntil).
// Обратите внимание: мера healthy не включает измерение inFlight (авторитетное решение автомата — только disabled/until/breakerUntil);
// inFlightFull — подмножество healthy: число healthy-аккаунтов с занятым транзитным лимитом, нужно /status для видимости загруженности.
// Отличие от ServableNow — см. комментарий той функции.
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow сообщает, обслуживает ли пул сейчас: есть хотя бы один healthy-аккаунт со свободными транзитными местами.
// От меры healthy в CountsDetailed отличается: healthy смотрит только на disabled/until/breakerUntil (авторитетное решение автомата),
// а не на inFlight; ServableNow дополнительно накладывает транзитное измерение, выравниваясь с реальной достижимостью chat (Pick пропускает inFlightFull-аккаунты).
// Только для /healthz, чтобы избежать расхождения мер: «все аккаунты healthy, но заняты» давало бы ложный 200 пробы при 503 от chat.
func (p *Pool) ServableNow() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if e.healthy(now) && !p.inFlightFull(e) {
			return true
		}
	}
	return false
}

// List возвращает состояния всех аккаунтов (сортировка по UID, стабильный вывод).
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}
func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	st := Status{
		UID:             uid,
		Nickname:        e.a.Nickname,
		Credits:         e.credits,
		Cooling:         now.Before(e.until) || now.Before(e.breakerUntil),
		Reason:          e.reason,
		Disabled:        e.disabled,
		SuccessCount:    e.successCount,
		ErrTotal:        e.errTotal,
		LastSuccessTime: e.lastSuccess,
		LastErrTime:     e.lastErr,
		Until:           e.until,
		SoftStreak:      e.softStreak,
		InFlight:        int(e.inFlight.Load()),
		BreakerFails:    e.fails,
		BreakerUntil:    e.breakerUntil,
	}
	if st.Disabled {
		// Отключённый аккаунт раскрывает причину отключения (иначе эксплуатации не видно, почему умер).
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		// Остаток кулдауна в секундах (округление вверх, чтобы 0 не выглядел истёкшим).
		st.CoolRemaining = int64(time.Until(e.until).Seconds() + 0.999)
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
		st.CoolKind = e.coolKind.String()
	}
	return st
}

// ---------------------------------------------------------------------------
// Персистентность
// ---------------------------------------------------------------------------
