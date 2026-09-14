// Выбор номера: семейство Pick (healthy трёхфакторное взвешивание Топ5 короткий список + взвешенный случайный + полный кулдаун-фолбэк + фильтр занятых транзитных мест).
package pool

import (
	"log"
	"math/rand/v2"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding — то же, но пропускает uid из tried (ротация уровня запроса).
// Стратегия выбора: из healthy-аккаунтов берём топ-5 по трёхфакторному весу, затем внутри Топ5 взвешенная случайная жеребьёвка по тому же весу,
// цель — размазать горячие точки, чтобы не бить вечно в один аккаунт.
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "")
}

// PickExcludingForModel — выбор номера с учётом модели: как PickExcluding, но для аккаунтов «в кулдауне 6004 уровня модели»
// действует освобождение модели — запрос с моделью, отличной от trigger-модели, считается доступным (issue #31).
// При пустом reqModel — обычный PickExcluding (существующая семантика вызовов не меняется).
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel)
}

// pick взвешенно случайно выбирает аккаунт из healthy-кандидатов по трёхфакторному весу и пишет lastUsed (против конкурентных столкновений).
// Набор кандидатов — приближение top5: сначала берём первые 5 по убыванию трёхфакторного веса (weightOf) (credits — лишь один фактор веса,
// компенсация простоя и успешность так же решают, кто попадёт в короткий список), затем внутри top5 фильтруем столкновения номеров.
// Защита от конкурентной лавины: пропускаем аккаунты с lastUsed ближе, чем minPickGap (кроме случая, когда весь top5 только что использован, —
// тогда откатываемся к давно не использовавшемуся LRU-аккаунту), заставляя высококонкурентные запросы расходиться, а не бить все в один топовый аккаунт.
// При minPickGap=0 (для тестов) фильтр всегда пропускает, деградация к чисто взвешенному случайному.
// При непустом reqModel мера здоровья меняется на healthyForModel (освобождение модели 6004 действует; PickExcluding передаёт "").
// Внимание: освобождение модели участвует только в normal-выборе (проверка healthy кандидатов); полный кулдаун-фолбэк в освобождении не участвует —
// фолбэк и есть деградация на случай «нет ни одного direct доступного», доступность со сменой модели уже учтена на normal-этапе.
func (p *Pool) pick(tried map[string]bool, reqModel string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	healthyOf := func(e *entry) bool { return e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue // Транзитные места заняты: пропускаем (при max=0 без ограничений не срабатывает)
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// Полный кулдаун-фолбэк: нет healthy-кандидатов — выбираем из охлаждаемых аккаунтов тот, у кого until истекает раньше
		// (прерывание/кулдаун делят меру expiry, берём более ранний срок). Отключённые аккаунты в фолбэке не участвуют никогда.
		return p.pickEarliestExpiryLocked(tried, now)
	}
	// Короткий список top5 усекаем по убыванию трёхфакторного веса (а не чистого убывания credits): иначе компенсация простоя + успешность
	// вообще не войдут в решение короткого списка, и аккаунты с низкими credits, но высокой успешностью/давним простоем никогда не попадут в top5.
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// Вес считаем один раз: усечение топ-5 требует сортировки, и вычисление weightOf прямо в компараторе sort раздулось бы до O(n log n)
	// избыточных float-вычислений (46 аккаунтов ≈ 500 раз). Сначала O(n) предрасчёт, затем сортировка по (весу, uid).
	type weighted struct {
		e *entry
		w float64
	}
	ws := make([]weighted, len(cands))
	for i, e := range cands {
		ws[i] = weighted{e: e, w: p.weightOf(e, maxCredits, now)}
	}
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	if len(cands) > 5 {
		cands = cands[:5]
	}
	eligible := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if now.Sub(e.lastUsed) >= minPickGap {
			eligible = append(eligible, e)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// Весь top5 только что использован: LRU-фолбэк, сохраняем расхождение и не морим голодом ни одного кандидата.
		e = cands[0]
		for _, c := range cands[1:] {
			if c.lastUsed.Before(e.lastUsed) {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible сохраняет порядок = убывающее подмножество top5
	}
	e.lastUsed = time.Now()
	return e.a
}

// pickEarliestExpiryLocked — полный кулдаун-фолбэк: среди неотключённых аккаунтов в мягком кулдауне/прерывании выбираем тот, чей срок раньше.
// Градация: disabled не участвуют никогда; CoolHard (баланс исчерпан, номер ждёт чекина) тоже исключаем — вызов даст гарантированный 402, трата ротации и шум в логах;
// CoolSoft и номера в прерывании допускаются (могли уже восстановиться, цена ошибки — лишь одна ротация).
// Аккаунты, исключённые tried, и с занятыми транзитными местами тоже пропускаем (держим ротацию уровня запроса + семантику аренды). Если доступных нет — nil.
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if e.disabled {
			continue // Отключённые аккаунты в фолбэке не участвуют никогда
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // Номер с исчерпанным балансом (в действующем hard-кулдауне) в фолбэке не участвует: ждёт восстановления чекином, вызов даст гарантированный 402
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry uid=%s until=%s kind=%s", logfmt.UID8(best.a.UID), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	return best.a
}

// inFlightFull сообщает, заняты ли все транзитные места аккаунта (max=0 без ограничений → всегда false).
// Вызывающий код уже должен держать p.mu (подойдёт замок на чтение или запись, метод только читает p.maxInFlight).
func (p *Pool) inFlightFull(e *entry) bool {
	if p.maxInFlight <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(p.maxInFlight)
}

// minPickGap — окно защиты от конкурентных столкновений номеров: один аккаунт в этом окне повторно не выбирается (кроме случая, когда весь top5 только что использован).
// В проде по умолчанию 100ms; для тестов чистого взвешенного распределения можно временно ставить 0 и выключать защиту.
var minPickGap = 100 * time.Millisecond

// pickWeighted — трёхфакторный взвешенный случайный выбор (ориентир claude-api selectWeightedRandom):
//
//		weight = доля credits × 10 + idleWeight + successRate × 3
//
//	  - доля credits = credits номера / максимальные credits среди кандидатов (чтобы не взрывать размерность)
//	  - idleWeight = min(часы с lastUsed × idleWeightPerHour, idleWeightMax); ни разу не использованный получает максимум
//	  - successRate = successCount/(successCount+errTotal); без истории запросов даём 1.5 (нейтрально-доверчиво)
//
// При credits все 0 взвешиваем всё равно по idle+successRate (без деградации к равномерному случайному).
// Вес — float, жеребьёвка в int64 с фиксированной точкой (×1e6) сохраняет инъекцию детерминированного источника случайности (семантика randInt64N не меняется).
// Источник случайности — в приоритете p.randInt64N (только для инъекции детерминированности в тестах), при nil откат к глобальному источнику math/rand/v2.
func (p *Pool) pickWeighted(cands []*entry) *entry {
	now := time.Now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	const scale = 1_000_000 // Фиксированная точка: суммируем веса в int64 и разыгрываем целым жребием
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.weightOf(e, maxCredits, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf вычисляет трёхфакторный вес одного аккаунта.
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	// 1. доля credits ×10 (с якорем mid-credit, чтобы при всех нулях слагаемое credits не было 0).
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 2. Компенсация простоя.
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // ни разу не использован → максимум
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed в будущем (часы переведены назад) — прижимаем к 0
		}
		w += idleW
	}
	// 3. Успешность ×3.
	totalReq := e.successCount + e.errTotal
	if totalReq > 0 {
		w += float64(e.successCount) / float64(totalReq) * 3
	} else {
		w += 1.5 // без истории запросов → нейтрально-доверчиво
	}
	return w
}

// SetCredits обновляет баланс аккаунта.
