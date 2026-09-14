// Кулдаун и прерыватель: Cooldown/CooldownSoftForModel (эскалация мягкого отката), потолок мягкого кулдауна,
// накопление ошибок прерывания, разморозка чекином (ReenableIfCredits/reviveCoolingLocked).
package pool

import (
	"time"
)

func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		p.dirty.Store(true)
	}
}

// Cooldown охлаждает аккаунт до now+d (мгновенный кулдаун: CoolSoft 429 / CoolHard нехватка баланса).
// Вход кулдауна — одновременно сигнал ошибки для прерывателя: кормит fails, по порогу прерывает с экспоненциальным откатом (ортогонально until).
//
// CoolSoft дополнительно делает **экспоненциальный откат последовательных мягких лимитов**: при последовательных мягких кулдаунах одного аккаунта фактическая длительность
// удваивается как d << (softStreak-1) (потолок softRateMax, см. softDurationLocked).
// Первый streak=1 → фактическая длительность = d, семантика одиночного вызова как в старом поведении.
// Это двойная эскалация параллельно с прерывателем, и это **намеренно**: мягкий откат отвечает за «недавно под лимитом»,
// раздувается от 600s до минут~часов; прерывание отвечает за «патологически повторяющиеся ошибки», банит надолго по 30m→1h→2h→6h.
// Путь кормления у обоих общий через этот вход, но счётчики независимы (softStreak vs fails), друг друга не загрязняют.
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if kind == CoolSoft {
			e.softStreak++
			d = p.softDurationLocked(d, e.softStreak)
		}
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// Вход не модельного кулдауна: стираем след освобождения модели 6004, чтобы прошлый лимит уровня модели
		// softRateModel не потёк в текущий лимит **уровня аккаунта** (иначе запрос со сменой модели ошибочно обойдёт этот кулдаун).
		e.softRateModel = ""
		p.recordBreakerFailureLocked(e) // вход кулдауна — тоже сигнал ошибки для прерывателя
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel — вход мягкого кулдауна **уровня модели** для 429 (issue #31).
// Отличие от Cooldown: когда апстрим 6004 прямо говорит «сброс будет в …», конец кулдауна точно ставим в
// resetAt (время, данное апстримом, а не угаданное из фиксированной базы+экспоненциального отката) и записываем вызвавшую модель softRateModel,
// после чего эта модель освобождается от кулдауна (смена модели сразу доступна, см. healthyForModel).
//
// Правила сужения:
//   - resetAt ненулевой (6004 с разобранным временем) → until = min(resetAt, now+softRateMax),
//     softRateModel = model. Экспоненциальный откат **не применяется**: время сброса уже авторитетно задано апстримом, дополнительное раздувание
//     проигнорировало бы прямо названный момент восстановления (в этом и была ключевая боль issue).
//   - resetAt нулевой (6004 без текста времени / soft не-6004) → полный откат к текущему Cooldown
//     (экспоненциальный откат soft_streak + потолок soft_rate_max), softRateModel остаётся пустым (без освобождения).
//
// Сигнал прерывания кормится как прежде (кулдаун и прерывание ортогональны, поведение как у Cooldown); softStreak всё равно растёт
// (независимо от попадания в разобранное время), единую консистентность держит семантика вне Cooldown: кулдаун
// с разобранным временем **не** участвует в экспоненциальном откате, но счёт softStreak штатно копится, и последующие 6004 без времени продолжают откат с текущего
// streak — вровень с заданием «логика экспоненциального отката не меняется, сужение только в двух точках».
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.softStreak++
		hasReset := !resetAt.IsZero()
		d := p.softDurationLocked(base, e.softStreak)
		if hasReset {
			// Есть время сброса от апстрима: берём прямо min(настенные часы сброса, now+softRateMax), без экспоненциального раздувания.
			now := time.Now()
			cap := now.Add(p.softRateMaxOr())
			if resetAt.After(cap) {
				d = cap.Sub(now)
			} else if resetAt.After(now) {
				d = resetAt.Sub(now)
			} else {
				// Время сброса уже прошло (сдвиг часов/протухший текст): кулдаун мизерный, сразу восстанавливаем.
				d = time.Millisecond
			}
		}
		e.until = time.Now().Add(d)
		e.coolKind = CoolSoft
		e.reason = reason
		if hasReset {
			e.softRateModel = model // модель записываем только для 6004 с разобранным временем (граница освобождения)
		} else {
			e.softRateModel = ""
		}
		p.recordBreakerFailureLocked(e) // вход кулдауна — тоже сигнал ошибки для прерывателя
		p.dirty.Store(true)
	}
}

// softRateMaxOr возвращает действующий softRateMax (если не внедрён — по умолчанию 2h) для расчёта потолка.
// Вызывающий код уже должен держать p.mu.
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked экспоненциально раздувает базу d по числу последовательных мягких кулдаунов: d << (streak-1), потолок softRateMax.
// Если softRateMax не внедрён (<=0), считаем по defaultSoftRateMax. При streak<=1 возвращаем d как есть.
// Число бит сдвига ограничено softStreakShiftMax, чтобы огромный streak не переполнил сдвиг.
// Вызывающий код уже должен держать p.mu.
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0: сдвиг переполнился в отрицательное/ноль — тоже страхуемся потолком
		d = max
	}
	return d
}

// recordBreakerFailureLocked копит одну ошибку прерывания; по порогу прерывает с экспоненциальным откатом.
// Прерывание отвязано от кулдауна (until): кулдаун даёт фиксированную длительность по классу ошибки, прерывание за «повторяющиеся ошибки» раз за разом удлиняет бан.
// Вызывающий код уже должен держать p.mu.
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// Срабатывание прерывания: сбрасываем счёт ошибок для нового накопления в следующем круге; retryCount растёт и раздувает показатель отката.
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM охлаждает до ближайших 04:00 (местный часовой пояс).
// Для сценария ErrHardCredit: аккаунт с исчерпанным балансом ждёт восстановления задачей чекина (09:00/21:00).
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM возвращает ближайшие 04:00 после now (тот же часовой пояс, что у now).
// Если now раньше сегодняшних 04:00 (ночь 00:00~04:00) — возвращаем сегодняшние 04:00: чекин ещё не выполнялся,
// жёсткий кулдаун в этом окне восстановится сегодняшним чекином; возврат завтрашнего дня заморозил бы впустую почти сутки.
// Ровно в 04:00 и позже — возвращаем 04:00 следующего дня.
// time.Date при переполнении дня сам переносится (конец месяца→1-е следующего, конец года→1-е следующего года), естественно покрывая сутки/месяц/год.
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// Disable — вечное отключение (смерть сессии), восстанавливается вручную после ручного перелогина или заменой файла.
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.softRateModel = "" // при обнулении домена кулдауна заодно стираем след освобождения модели
}

// ReenableIfCredits — разморозка после чекина: только если remain > 0 и аккаунт не отключён, снимаем кулдаун (баланс восстановлен).
// Внимание: прерыватель не трогаем — восстановление только по истечении прерывания (breakerUntil истёк) или при следующем успехе chat (NoteSuccess).
