// Package pool — пул аккаунтов: единый автомат состояний (здоров/кулдаун/прерыватель) + транзитные аренды + трёхфакторный взвешенный выбор + персистентность state.json.
package pool

import (
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type CoolKind int

const (
	CoolHard CoolKind = iota // нехватка баланса → кулдаун до 04:00 следующего дня (ждём восстановления чекином)
	CoolSoft                 // 429 → короткий кулдаун
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	}
	return "unknown"
}

// Status — внешне видимое состояние одного аккаунта (десенсибилизировано).
type Status struct {
	UID             string    `json:"uid"`
	Nickname        string    `json:"nickname,omitempty"`
	Credits         int64     `json:"credits"`
	Cooling         bool      `json:"cooling"`
	CoolKind        string    `json:"cool_kind,omitempty"`
	CoolRemaining   int64     `json:"cool_remaining_sec,omitempty"`
	Until           time.Time `json:"until,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	SoftStreak      int       `json:"soft_streak,omitempty"` // число последовательных мягких кулдаунов (показатель экспоненциального отката, см. entry.softStreak)
	Disabled        bool      `json:"disabled"`
	DisabledReason  string    `json:"disabled_reason,omitempty"` // только для disabled-аккаунтов: причина отключения (видна эксплуатации)
	SuccessCount    int64     `json:"success_count,omitempty"`
	ErrTotal        int64     `json:"err_total,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime     time.Time `json:"last_err,omitempty"`
	// Рабочее состояние (не сохраняется): число транзитных запросов + состояние прерывателя.
	InFlight     int       `json:"in_flight"`
	BreakerFails int       `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}
type entry struct {
	a            *auth.Auth
	credits      int64
	successCount int64     // накопленные успехи
	errTotal     int64     // накопленные ошибки (для веса успешности successRate = successCount/(successCount+errTotal), не обнуляется)
	lastErr      time.Time // время последней ошибки
	lastSuccess  time.Time // время последнего успеха
	coolKind     CoolKind
	until        time.Time // конец кулдауна (мгновенный кулдаун: CoolSoft 429 / CoolHard нехватка баланса)
	disabled     bool
	reason       string
	lastUsed     time.Time // момент последнего выбора (защита от конкурентных столкновений)
	// breakerUntil / fails / retryCount — рабочее состояние прерывателя (не сохраняется).
	// fails — единственный счётчик «последовательных ошибок»: любая ошибка его кормит, по достижении breakerThreshold срабатывает прерыватель (экспоненциальный откат),
	// накапливается через все входы, обнуляется при успехе/срабатывании/едином возрождении (retryCount сохраняется и движет показателем отката).
	breakerUntil time.Time // конец прерывания (экспоненциальный откат)
	fails        int       // счёт последовательных ошибок (для прерывателя, единственный авторитетный)
	retryCount   int       // число срабатываний прерывателя (показатель экспоненциального отката)
	// softStreak — число последовательных мягких кулдаунов (CoolSoft), счётчик **домена кулдауна**, независимый от fails прерывателя:
	// fails обнуляется срабатыванием прерывателя и загрязняется hard-кулдауном и NoteError, поэтому «последовательный мягкий лимит» выразить не может.
	// Точек сброса всего две (обе — моменты доказанного восстановления аккаунта): NoteSuccess, reviveCoolingLocked.
	// Сохраняется (stateAccount.SoftStreak): после рестарта мягкий лимит остаётся в откате, не возвращается к базе.
	softStreak int
	// softRateModel — имя модели, вызвавшей лимит 6004 уровня модели (освобождение модели в issue #31).
	// Записывается, только если кулдаун вызван «6004 с разобранным временем»; пусто = обычный мягкий кулдаун (без освобождения).
	// Семантика рабочего состояния (не сохраняется): рестарт обнуляет, деградация к текущему поведению.
	softRateModel string
	// sessionDeadFails — счёт последовательных 12153 (ErrSessionDead). В реальной среде 12153 срабатывает и эпизодически
	// (сетевой джиттер/мигание апстрима/гонка refresh) — вечное отключение за одну ошибку слишком грубо, смерть признаём только по достижении порога подряд.
	// Семантика рабочего состояния (не сохраняется, как у inFlight): обнуление при рестарте приемлемо — первый keepalive после рестарта
	// при успехе сбрасывает счёт, ошибочно осуждённый номер не будет догонять накопленная до рестарта история.
	sessionDeadFails int
	// inFlight — число транзитных запросов одного аккаунта (рабочее состояние, не сохраняется). atomic, чтобы горячий путь Pick не брал замок на запись.
	inFlight atomic.Int64
}

// healthy сообщает, выбираем ли аккаунт сейчас (не отключён, не в кулдауне/прерывании).
func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	return true
}

// healthyForModel сообщает, выбираем ли аккаунт для указанной model (с освобождением уровня модели):
// Если кулдаун — **уровня модели**, вызванный 6004 (softRateModel непуст), а запрошенная модель другая
// (softRateModel != reqModel), проверка кулдауна пропускается — лимит этой модели не означает, что аккаунт недоступен на других
// моделях (issue #31). Пустой reqModel / немаркированная модель / та же модель → как healthy.
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	if !e.healthy(now) && reqModel != "" && e.softRateModel != "" &&
		e.coolKind == CoolSoft && e.softRateModel != reqModel {
		// Не healthy, но случай под освобождение: ограничения disabled/breakerUntil всё равно действуют.
		return !e.disabled && e.breakerUntil.IsZero()
	}
	return e.healthy(now)
}

// expiry возвращает ближайший действующий срок кулдауна/прерывания аккаунта (из двух сроков — более ранний); вне кулдауна — нулевое значение.
// Нужно полному кулдаун-фолбэку для выбора аккаунта с «самым ранним истечением».
func (e *entry) expiry(now time.Time) time.Time {
	var t time.Time
	if !e.until.IsZero() && now.Before(e.until) {
		t = e.until
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if t.IsZero() || e.breakerUntil.Before(t) {
			t = e.breakerUntil
		}
	}
	return t
}

// fallbackKind сообщает, к какому классу кулдауна относится фолбэк-аккаунт (soft: мгновенный мягкий кулдаун; breaker: период прерывания).
// Вызывать только для аккаунтов-участников фолбэка (CoolHard уже исключён pickEarliestExpiryLocked). Правило решения:
// Если срок прерывания — текущий ближайший действующий срок (включая «только прерывание без мягкого кулдауна»), считаем breaker; иначе soft.
func (e *entry) fallbackKind(now time.Time) string {
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if e.until.IsZero() || !now.Before(e.until) || e.breakerUntil.Before(e.until) {
			return "breaker"
		}
	}
	return "soft"
}

// stateAccount — сохраняемое состояние одного аккаунта (JSON-теги строчными с подчёркиванием, обратная совместимость: пропущенные поля — ноль).
type stateAccount struct {
	Credits      int64     `json:"credits"`
	Disabled     bool      `json:"disabled"`
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	CoolKind     CoolKind  `json:"cool_kind"`
	SuccessCount int64     `json:"success_count,omitempty"`
	// err_total — накопленный счёт ошибок. Старый err_count (последовательные ошибки) всё ещё читается: при загрузке отображается в err_total,
	// только как разовая миграция, обратно err_count больше не пишется.
	ErrTotal    int64     `json:"err_total,omitempty"`
	ErrCount    int       `json:"err_count,omitempty"` // источник миграции для совместимости со старыми файлами, только чтение
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr     time.Time `json:"last_err,omitempty"`
	// SoftStreak — число последовательных мягких кулдаунов (показатель мягкого отката). В старом state.json поля нет → ноль,
	// откат начинается заново с базы (обратная совместимость).
	SoftStreak int `json:"soft_streak,omitempty"`
}

// stateFile — формат сохранения.
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval — период фоновой записи на диск.
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax — потолок экспоненциального отката мягкого кулдауна по умолчанию: если softRateMax не внедрён (<=0), считаем по нему,
// чтобы в тестах/голом пуле откат не был безграничным.
const defaultSoftRateMax = 2 * time.Hour

// sessionDeadThreshold — вечное отключение только после этого числа последовательных ErrSessionDead (12153).
// 12153 срабатывает эпизодически (сетевой джиттер/мигание апстрима/гонка refresh), старое поведение «отключение за одну ошибку»
// убивало здоровые аккаунты (P0-1: все 13 disabled-номеров — ошибочные). Смерть только после 3 подряд: терпим случайный джиттер,
// но не даём по-настоящему мёртвой сессии крутиться в пуле и выбираться снова.
const sessionDeadThreshold = 3

// sessionDeadReason — сохраняемый reason, когда 12153 признан смертью сессии.
const sessionDeadReason = "12153 session dead"

// SessionDeadThreshold раскрывает порог отключения по последовательным 12153 (для логов scheduler/эксплуатационной документации).
func SessionDeadThreshold() int { return sessionDeadThreshold }

// softStreakShiftMax — максимальный сдвиг влево для мягкого отката (защита от переполнения 1<<streak в отрицательное/ноль).
// Сколько бы streak ни накопился, логика потолка всегда срабатывает раньше, это значение — лишь страховка от переполнения.
const softStreakShiftMax = 16

// StoreSnapshotter — минимальный интерфейс зеркала снапшотов состояния пула (redisstore.Store удовлетворяет; пустая Noop-реализация безопасна).
// Сосуществует с локальным state.json как бэкап восстановления при старте: снапшот берётся, только если он новее локального, иначе приоритет у локального.
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax     = 5.0
)

// New строит пул; при непустом stateFp пытается загрузить старое состояние и запускает фоновую goroutine периодической записи на диск.
