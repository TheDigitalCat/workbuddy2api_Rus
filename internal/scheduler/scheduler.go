// Package scheduler — плановые задачи: подписание / отчёт об активности / путешествие питомца / token keepalive, четыре независимых расписания.
// После успешного подписания заново запрашиваем баланс, охлаждаемые номера с балансом > 0 автоматически размораживаем.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config — зависимости планировщика.
//
// Переключатели задач названы через «отключение», а не «включение»: нулевой Config означает все четыре задачи включены (hours откатываются к умолчанию),
// поведение побуквенно совпадает с эпохой до переключателей (старым вызывающим/тестам меняться не надо).
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // по умолчанию [9, 21]
	TravelHours    []int // по умолчанию [9,21]: одна отправка + одно получение закрывают цикл
	ActivityHours  []int // по умолчанию [10]
	KeepaliveHours []int // по умолчанию [22]
	// ActivityReportCount — число отчётов активности на номер за раз: взять питомца требует 5 диалогов,
	// по умолчанию 5 отчётов в одном conversationId многходовкой добивают chat_5; 0/по умолчанию=1 ради совместимости со старым поведением.
	ActivityReportCount int

	// CheckinDisabled явно выключает расписание подписания (соответствует schedule.checkin_enabled=false в config).
	// После отключения точек подписания больше нет. Путешествие больше не едет зайцем на подписании (выделено в независимое расписание).
	CheckinDisabled bool
	// TravelDisabled явно выключает расписание путешествий питомца (schedule.travel_enabled=false).
	TravelDisabled bool
	// ActivityDisabled явно выключает расписание отчётов активности (schedule.activity_enabled=false).
	ActivityDisabled bool
	// KeepaliveDisabled явно выключает расписание поддержания token (schedule.keepalive_enabled=false).
	KeepaliveDisabled bool
}

// Scheduler — планировщик.
type Scheduler struct {
	cfg Config

	// mu/adoptTried — журнал дневных неудач взятия: uid → календарный день (CST). Номера, не прошедшие порог, в этот день больше не пробуем,
	// чтобы не бомбардировать апстрим повторами несколько раз в день; при рестарте процесса обнуляется (персистентность не нужна).
	mu         sync.Mutex
	adoptTried map[string]string

	// checkinMu сериализует подписания: плановый вход и ручной запуск взаимоисключают друг друга, чтобы не бить интерфейс подписания апстрима дважды в один момент.
	checkinMu sync.Mutex
}

// New создаёт.
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	// 0/по умолчанию = 1 отчёт (совместимость со старым: один отчёт в день на номер поддерживает серию входов).
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	return &Scheduler{cfg: cfg, adoptTried: make(map[string]string)}
}

// checkinRefreshSkew — окно «истекает ли token скоро» перед подписанием (10 минут).
// После долгого простоя/долгой остановки контейнера access token обычно уже протух, без предварительного обновления подписание впустую получит 401.
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus — статус результата подписания одного номера.
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // подписание успешно
	CheckinAlready CheckinStatus = "already" // апстрим решил, что сегодня уже подписано (идемпотентный повтор, считаем нормой)
	CheckinFail    CheckinStatus = "fail"    // не удалось обновить token / подписаться / запросить баланс
	CheckinSkipped CheckinStatus = "skipped" // номер отключён или нет валидных учётных данных, не участвовал
)

// CheckinOutcome — результат подписания одного номера (для ручной квитанции и сводки логов).
type CheckinOutcome struct {
	UID      string         `json:"uid"`
	Nickname string         `json:"nickname,omitempty"`
	Status   CheckinStatus  `json:"status"`
	Credits  *int64         `json:"credits,omitempty"` // баланс после подписания (есть только при успешном запросе баланса)
	Detail   string         `json:"detail,omitempty"`  // причина неудачи/пропуска (цитату апстрима «已签到» не заполняем)
}

// ErrBusy — подписание уже выполняется (ручной вход столкнулся с плановым).
var ErrBusy = errors.New("подписание уже выполняется")

// nextFire возвращает ближайшее после now целочасовое время срабатывания; hours — локальные часы (0-23).
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind — тип плановой задачи.
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
)

// nextWake возвращает ближайшее после now время пробуждения и все задачи, которые в этот момент надо выполнить.
// Если несколько классов задач назначены на один час (например подписание и путешествие содержат 9), в этот момент выполняются все.
// Явно отключённые задачи в кандидаты не попадают (nextFire для них возвращает нулевое время, nextWake нулевые точки пропускает).
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !s.cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !s.cfg.TravelDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.TravelHours), taskTravel})
	}
	if !s.cfg.ActivityDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.ActivityHours), taskActivity})
	}
	if !s.cfg.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// Run — главный цикл, блокируется до отмены ctx.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// Все четыре класса задач отключены: не крутимся вхолостую, только ждём сигнал выхода.
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// Задачи на точку фиксируются при планировании (не зависят от часа момента пробуждения), опоздавшее пробуждение ничего не пропустит.
			for _, k := range kinds {
				switch k {
				case taskCheckin:
					s.RunCheckinNow()
				case taskTravel:
					s.RunTravelNow()
				case taskActivity:
					s.RunActivityNow()
				case taskKeepalive:
					s.RunKeepaliveNow()
				}
			}
		}
	}
}

// RunCheckinNow — немедленное подписание по таймеру: пономерные итоги логирует CheckinAll, здесь лишь ловим «пропуск из-за столкновения».
func (s *Scheduler) RunCheckinNow() {
	if _, err := s.CheckinAll(); err != nil {
		log.Printf("плановое подписание пропущено: %v", err)
	}
}

// CheckinAll — полное подписание: обновление token по нужде → daily-checkin → запрос баланса → разморозка охлаждаемых номеров.
// Охлаждаемые номера тоже участвуют (подписание и нужно, чтобы их разморозить); отключённые пропускаем.
// В один момент разрешено только одно подписание, повторный вызов возвращает ErrBusy (чтобы ручной запуск и таймер не били апстрим дважды).
//
// session dead идёт по семантике **непрерывного счёта** Pool.NoteSessionDead (как keepalive):
// одна неудача обновления больше не убивает номер сразу, отключаем после подряд sessionDeadThreshold раз, успех обновления обнуляет счёт.
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// Если простой пересёк срок жизни token (ночная остановка/долгий простой контейнера) — сначала догоняющее обновление, иначе подписание впустую получит 401.
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin %s обновление: %v", logfmt.UID8(st.UID), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: %d раз подряд 12153 session dead — отключение", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
					}
				}
				// Обновление — лишь «досрочный добор билета»: если token ещё валиден, подписываемся как обычно (иначе джиттер интерфейса обновления
				// зря пропустил бы подписание, которое могло успеть); неудачей считаем только истинное истечение.
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else if err := a.SaveAtomic(); err != nil {
				// Обновили успешно, но на диск не сохранили: после рестарта будет старый token — такое надо показать.
				log.Printf("checkin %s сохранение: %v", logfmt.UID8(st.UID), err)
			}
		}
		// При ошибке подписания (включая цитату апстрима «今天已签到») всё равно запрашиваем баланс: восстановился — номер размораживаем.
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				// цитата апстрима «今天已签到» — идемпотентный успех, а не ошибка: detail не заполняем, чтобы в квитанции
				// не красовался целый кусок 400-ответа, который прочтут как провал подписания.
				oc.Status = CheckinAlready
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.UID8(st.UID), err)
			}
		} else {
			oc.Status = CheckinOK
		}
		remain, err := s.cfg.Upstream.UserResource(a)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.UID8(st.UID), err)
			oc.Status = CheckinFail
			oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("подписание завершено: всего=%d успешно=%d уже=%d ошибок=%d пропущено=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// joinDetail склеивает несколько фрагментов причины, чтобы поздний не затирал раннюю информацию о неудаче.
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// RunActivityNow немедленно выполняет отчёт диалоговой активности по всем доступным номерам пула.
// Отключённые номера пропускаем; без AccessToken пропускаем; между номерами — лимит скорости activityAccountDelay.
//
// Каждый номер отчитывается N раз (ActivityReportCount, по умолчанию 5): все N делят один conversationId
// (wb2api-<ms>), имитируя N кругов диалога в одной сессии, — это намеренный приём для порога диалогов взятия питомца (buddy/first)
// (пререквизит chat_5 требует 5 диалогов). requestId у каждого свой (многходовка одной сессии).
// Между N отчётами одного номера — пауза activityReportGap (1.5s), чтобы мгновенная отправка не триггерила антифрод.
//
// 0/по умолчанию ActivityReportCount = 1 отчёт, совместимость со старым (только поддержать серию входов + разблокировать first_buddy).
//
// После успешного отчёта: ① самопроверка streak (перечитываем серию входов, ловим «200, но молча выброшено»);
// ② номера без питомца сразу повторяют взятие (travelAdoptForce) — объём диалогов только что добрали, это новое состояние, а не повтор,
// освобождаем от дневного дебаунса adoptTriedToday (путешествие в 09:00 уже пыталось взять и пропустило, в 10:00 отчёт добрался
// и ждать следующего круга путешествия нельзя — замыкаем на месте).
func (s *Scheduler) RunActivityNow() {
	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if !first {
			time.Sleep(activityAccountDelay)
		}
		first = false
		// Все N делят один conversationId (одна сессия), requestId у каждого свой (по одному на отчёт).
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: отчёт %d/%d: %v", logfmt.UID8(a.UID), i, count, err)
				break // отчёт этого номера не ушёл: дальше не шлём, самопроверка streak бессмысленна
			}
			log.Printf("activity %s: отчёт %d/%d ok", logfmt.UID8(a.UID), i, count)
			ok++
			if i < count {
				time.Sleep(activityReportGap) // пауза между 5 отчётами номера, чтобы мгновенная отправка не триггерила антифрод
			}
		}
		if ok < count {
			continue // N отчётов не добрали: самопроверка streak и взятие бессмысленны, следующий номер
		}
		s.checkActivityStreak(a) // все N ушли → перечитываем streak для самопроверки (в лог только итог)
		s.travelAdoptForce(a)    // у номера без питомца объём диалогов только добрали → сразу повторяем взятие (без дебаунса)
	}
}

// checkActivityStreak после успешного отчёта перечитывает дни серии входов (только чтение oracle, ловит молчаливые потери).
// Контекст: REPORT-active-map.md §2, замер «отчёт 200, но молча выброшен» (без userId progress не двигается),
// отчёт 200 ≠ зачёт streak — нужна замкнутая проверка перечитыванием.
// Порог аномалии: days==0 → warn (отчёт OK, но streak.days=0 (молчаливый сброс?));
// GET не удался → warn, но главный поток не трогаем (сам отчёт уже успешен, идемпотентен по дню, повторов нет).
// Лог — одна строка на номер, удобно grep'ать: `activity %s: streak дней=%d` (успех тоже пишем, для сверки).
// Возвращает true, если «отчёт OK, но streak подозрителен» (days==0 или перечитать не вышло), для assert'ов тестов.
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: проверка streak не удалась (отчёт OK): %v", logfmt.UID8(a.UID), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: отчёт OK, но streak.days=0 (молчаливый сброс?)", logfmt.UID8(a.UID))
		return true
	}
	log.Printf("activity %s: streak дней=%d", logfmt.UID8(a.UID), days)
	return false
}

// RunKeepaliveNow немедленно обновляет token всех номеров; умершие сессии автоматически отключаем.
// Отключение по 12153 идёт по семантике **непрерывного счёта** Pool.NoteSessionDead: одна неудача обновления номер сразу не убивает,
// отключаем после подряд sessionDeadThreshold раз (3 раза) (P0-1: все 13 отключённых номеров были историческими ложными срабатываниями).
// Успех обновления → ClearSessionDead обнуляет счёт (у ошибочно осуждённых номеров есть путь к возрождению).
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.UID8(st.UID), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: %d раз подряд 12153 session dead — отключение", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // успех обновления чистит счёт ложных срабатываний, неудача копиться не должна
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s сохранение: %v", logfmt.UID8(st.UID), err)
		}
	}
}
