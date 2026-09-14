// travel.go — автомат обхода путешествий питомца: в точки путешествий (travel_hours, по умолчанию 09:00) каждый доступный номер пула продвигаем на одну поездку.
// Нет питомца → согласие с соглашением + взятие; есть питомец → по travel/status распределяем отправку / получение / пропуск.
package scheduler

import (
	"log"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

const (
	// travelLocationID — место отправки фиксировано 4 (постоялый двор старого города): у всех 4 мест интервалы награды/длительности полностью одинаковы, оптимума нет.
	travelLocationID = 4

	// travelStateIdle — свободен, можно отправлять; travelStateTraveling — в пути; travelStateArrived — прибыл, можно забирать награду.
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay — лимит скорости между номерами: все номера примерно за 40s, чтобы не триггерить антифрод апстрима. В тестах можно ставить 0.
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay — лимит скорости отчётов активности между номерами: тот же калибр, что у путешествий, от антифрода апстрима. В тестах можно ставить 0.
var activityAccountDelay = 800 * time.Millisecond

// activityReportGap — пауза между последовательными отчётами одного номера: серией из 5 имитируем многходовку одной сессии,
// мгновенная отправка легко триггерит антифрод, поэтому 1.5s на отчёт. В тестах можно ставить 0.
var activityReportGap = 1500 * time.Millisecond

// cstZone — суточный сброс апстрима идёт по календарному дню 00:00 CST (Asia/Shanghai). В Китае нет летнего времени, фиксированных +8 достаточно,
// tzdata контейнера не нужна.
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay возвращает календарный день апстрима (CST), к которому относится t, в формате 2006-01-02.
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow немедленно выполняет один круг обхода путешествий по всем доступным номерам пула.
// Отключённые номера пропускаем; 401/неудача запроса пропускает только этот круг номера (token насильно не обновляем, это дело keepalive в 22:00);
// между номерами — лимит скорости travelAccountDelay.
func (s *Scheduler) RunTravelNow() {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		s.travelOne(a)
	}
}

// travelOne — автомат одного номера на одну поездку: есть ли питомец + статус + максимум одно действие, без опроса и ожидания.
func (s *Scheduler) travelOne(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: запрос питомца: %v", logfmt.UID8(a.UID), err)
		return
	}
	if buddy == nil {
		s.travelAdopt(a)
		return
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: статус: %v", logfmt.UID8(a.UID), err)
		return
	}
	switch ts.State {
	case travelStateArrived:
		s.travelClaim(a, ts)
	case travelStateIdle:
		s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: пропуск (в пути record=%d)", logfmt.UID8(a.UID), ts.RecordID)
	default:
		log.Printf("travel %s: пропуск (неизвестное состояние %q)", logfmt.UID8(a.UID), ts.State)
	}
}

// travelDepart отправляет, если свободен и дневной лимит не исчерпан (1 раз в день, сброс в 00:00 CST календарного дня).
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) {
	if ts.DailyLimitReached {
		log.Printf("travel %s: пропуск (дневной лимит исчерпан)", logfmt.UID8(a.UID))
		return
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: отправка: %v", logfmt.UID8(a.UID), err)
		return
	}
	log.Printf("travel %s: отправка выполнена location=%d", logfmt.UID8(a.UID), travelLocationID)
}

// travelClaim забирает награду по прибытии (обязателен record_id).
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) {
	if ts.RecordID == 0 {
		log.Printf("travel %s: получение пропущено (прибыл, но нет record_id)", logfmt.UID8(a.UID))
		return
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: получение record=%d: %v", logfmt.UID8(a.UID), ts.RecordID, err)
		return
	}
	log.Printf("travel %s: получение выполнено record=%d reward=%d", logfmt.UID8(a.UID), ts.RecordID, reward)
}

// travelAdopt — взятие при обходе путешествий: связано дневным дебаунсом adoptTriedToday.
func (s *Scheduler) travelAdopt(a *auth.Auth) {
	s.adoptBuddy(a, false)
}

// travelAdoptForce — взятие после того, как отчёт активности добрад объём диалогов: освобождено от дневного дебаунса adoptTriedToday.
// Контекст: путешествие в 09:00 уже пыталось взять, но объём диалогов не дотянул, и круг пропустили; отчёт активности в 10:00 серией из 5
// объём добрад — это новое состояние «порог только что взят», а не бомбардировка апстрима повторами, повтор разрешаем.
// Если у номера уже есть питомец (BuddyInfo непуст) — сразу пропускаем (повторно не берём).
func (s *Scheduler) travelAdoptForce(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("activity %s: запрос питомца: %v", logfmt.UID8(a.UID), err)
		return
	}
	if buddy != nil {
		return // питомец уже есть, брать не надо
	}
	s.adoptBuddy(a, true) // force=true освобождает от дневного дебаунса
}

// adoptBuddy берёт при отсутствии питомца: сначала согласие с соглашением (идемпотентно), затем buddy/first.
// Недобор порога conversation (HTTP 400 first_buddy task not completed yet) — ожидаемое поведение,
// записываем дневную попытку и молча пропускаем, больше не повторяем. При force=true дневной дебаунс снят (повтор после добра объёма диалогов отчётом активности).
func (s *Scheduler) adoptBuddy(a *auth.Auth, force bool) {
	if !force && s.adoptTriedToday(a.UID) {
		return
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: соглашение: %v", logfmt.UID8(a.UID), err)
		return
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: взятие выполнено (+300 credits)", logfmt.UID8(a.UID))
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: взятие пропущено (порог диалогов не взят, повтор завтра)", logfmt.UID8(a.UID))
	default:
		log.Printf("travel %s: взятие: %v", logfmt.UID8(a.UID), err)
	}
}

// adoptTriedToday — решено ли сегодня, что номер не прошёл порог взятия.
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried записывает, что номер сегодня уже пытался взять и порог не прошёл.
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
