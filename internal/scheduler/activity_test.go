package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// fastActivity глушит межномерной лимит и интервал отчётов активности, чтобы тесты не ждали вхолостую.
func fastActivity(t *testing.T) {
	t.Helper()
	oldDelay := activityAccountDelay
	oldGap := activityReportGap
	activityAccountDelay = 0
	activityReportGap = 0
	t.Cleanup(func() {
		activityAccountDelay = oldDelay
		activityReportGap = oldGap
	})
}

// reportStub считает вызовы /v2/report и userId.
type reportStub struct {
	calls  atomic.Int32
	uids   atomic.Int32
	bodies atomic.Int32
}

func (s *reportStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			s.calls.Add(1)
			uid := r.Header.Get("X-User-Id")
			if uid != "" {
				s.uids.Add(1)
			}
			s.bodies.Add(1) // помечаем, что body пришёл (наличие userId в массиве assert'ов покрыто юнит-тестами пакета upstream)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
			return
		}
		http.Error(w, "not found", 404)
	})
}

// TestRunActivityNowReportsEachAccount: по каждому доступному номеру пула отчитываемся один раз.
func TestRunActivityNowReportsEachAccount(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow()

	if n := stub.calls.Load(); n != 2 {
		t.Errorf("report calls=%d want 2 (по одному отчёту на номер)", n)
	}
	if n := stub.uids.Load(); n != 2 {
		t.Errorf("report with X-User-Id=%d want 2 (каждый номер обязан нести userId)", n)
	}
}

// TestRunActivityNowSkipsDisabledAndNoToken: отключённые номера и номера без token пропускаем.
func TestRunActivityNowSkipsDisabledAndNoToken(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "notoken", AccessToken: "", RefreshToken: "", ExpiresAt: 9999999999})
	p.Disable("dis", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow()

	if n := stub.calls.Load(); n != 1 {
		t.Errorf("report calls=%d want 1 (только ok-номер)", n)
	}
}

// TestRunActivityNowErrorDoesNotAbort: провал отчёта одного номера не влияет на обход остальных.
func TestRunActivityNowErrorDoesNotAbort(t *testing.T) {
	fastActivity(t)
	var okCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			// streak-самопроверку и прочие не-report запросы сразу отвечаем 200 (без счётчика, считаем только /v2/report).
			w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		uid := r.Header.Get("X-User-Id")
		if uid == "fail" {
			w.WriteHeader(500)
			w.Write([]byte(`boom`))
			return
		}
		okCalls.Add(1)
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "fail", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow() // не должен паниковать

	if n := okCalls.Load(); n != 1 {
		t.Errorf("ok account report calls=%d want 1 (провальный номер не влияет на обход остальных)", n)
	}
}

// ---------------------------------------------------------------------------
// P1: после отчёта активности перечитываем streak-самопроверку
// ---------------------------------------------------------------------------

// activityStreakStub имитирует /v2/report (200 успех) + /activity/growth/streak (days настраиваем).
type activityStreakStub struct {
	reportCalls atomic.Int32
	days        int  // дней серии входов, возвращаемых streak
	streakErr   bool // заставить streak вернуть 500
	noUserId    bool // кейс: отчёт без userId (сервер 200, но молча выкидывает)
	streakHits  atomic.Int32
}

func (s *activityStreakStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			s.reportCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/activity/growth/streak":
			s.streakHits.Add(1)
			if s.streakErr {
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"streak":{"days":%d}}}`, s.days)
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// activityStreakScheduler собирает планировщик со stub'ом streak-самопроверки.
func activityStreakScheduler(t *testing.T, srv *httptest.Server) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestRunActivityNowSelfCheckDaysNormal: после успешного отчёта перечитываем streak: days>=1 — без алерта.
func TestRunActivityNowSelfCheckDaysNormal(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{days: 3}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow()
	if stub.reportCalls.Load() != 1 || stub.streakHits.Load() != 1 {
		t.Errorf("report_calls=%d streak_hits=%d want 1/1", stub.reportCalls.Load(), stub.streakHits.Load())
	}
	// days>=1: checkActivityStreak возвращает false (ничего подозрительного).
	if s.checkActivityStreak(p.AuthByUID("u1")) {
		t.Fatal("days>=1 не должен алертить")
	}
}

// TestRunActivityNowSelfCheckSilentDrop: отчёт 200, но streak.days=0 — алертим (silent drop?).
func TestRunActivityNowSelfCheckSilentDrop(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{days: 0}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	if !s.checkActivityStreak(p.AuthByUID("u1")) {
		t.Fatal("days=0 должен алертить (отчёт 200, но silent drop?)")
	}
}

// TestRunActivityNowSelfCheckGETFailure: перечитывающий GET провалился — алертим, но главный поток не трогаем (отчёт уже успешен).
func TestRunActivityNowSelfCheckGETFailure(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{days: 1, streakErr: true}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	// Прямо assert'им checkActivityStreak: GET провалился — алертим.
	if !s.checkActivityStreak(p.AuthByUID("u1")) {
		t.Fatal("провал streak GET должен алертить (reported but unverifiable)")
	}
	// Перечитывание — read-only oracle: провал GET не влияет на уже случившийся отчёт этого круга (обход продолжается).
	if stub.streakHits.Load() != 1 {
		t.Errorf("streak_hits=%d want 1 (провал GET всё равно бьёт в streak)", stub.streakHits.Load())
	}
}

// TestRunActivityNowSkipsSelfCheckOnReportFail: отчёт провалился — самопроверку не гоняем (SKIP, перечитывать бессмысленно).
func TestRunActivityNowSkipsSelfCheckOnReportFail(t *testing.T) {
	fastActivity(t)
	var streakHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			w.WriteHeader(500)
			w.Write([]byte(`boom`))
		case "/activity/growth/streak":
			streakHits.Add(1)
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow() // отчёт не успешен — самопроверки нет
	if streakHits.Load() != 0 {
		t.Errorf("streak hits=%d want 0 (провал отчёта — самопроверки нет)", streakHits.Load())
	}
}

// ---------------------------------------------------------------------------
// Серия из 5 отчётов + связка со взятием питомца
// ---------------------------------------------------------------------------

// activityBurstStub пишет requestId/conversationId каждого /v2/report и имитирует путь взятия питомца
// (buddy/info + agreement + buddy/first) и streak-самопроверку.
type activityBurstStub struct {
	reportBodies []map[string]any // event каждого отчёта
	infoCalls    atomic.Int32
	firstCalls   atomic.Int32
	agreeCalls   atomic.Int32
}

func (s *activityBurstStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			var arr []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&arr)
			if len(arr) > 0 {
				s.reportBodies = append(s.reportBodies, arr[0])
			}
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/activity/growth/buddy/info":
			s.infoCalls.Add(1)
			// Нет питомца — уходим в путь взятия
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case "/activity/growth/buddy/agreement":
			s.agreeCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"agreed":true}}`))
		case "/activity/growth/buddy/first":
			s.firstCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"档案喵"}}}`)) // данные: имя питомца-фикстуры (значение name), не переводим
		case "/activity/growth/streak":
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// TestRunActivityNowBurstSharedCIDIndependentRID: серия из 5 — общий conversationId,
// requestId у каждого свой (многоходовка одной сессии); после успеха всех 5 — лишь одна streak-самопроверка.
func TestRunActivityNowBurstSharedCIDIndependentRID(t *testing.T) {
	fastActivity(t)
	stub := &activityBurstStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 5})

	s.RunActivityNow()

	if n := len(stub.reportBodies); n != 5 {
		t.Fatalf("report bodies=%d want 5", n)
	}
	// Все 5 делят один conversationId.
	cid := stub.reportBodies[0]["conversationId"]
	for i, ev := range stub.reportBodies {
		if ev["conversationId"] != cid {
			t.Errorf("event %d conversationId=%v want %v (должны делить одну сессию)", i, ev["conversationId"], cid)
		}
	}
	// requestId у каждого свой.
	seen := map[any]bool{}
	for i, ev := range stub.reportBodies {
		rid := ev["requestId"]
		if rid == cid {
			t.Errorf("event %d requestId == conversationId (должен быть независимым)", i)
		}
		if seen[rid] {
			t.Errorf("event %d requestId=%v дублируется (у каждого должен быть свой)", i, rid)
		}
		seen[rid] = true
	}
}

// TestRunActivityNowBurstTriggersAdopt: номер без питомца после серии из 5 сразу повторяет взятие:
// buddy/first вызван и вернул ok (освобождён от дневного дебаунса adoptTriedToday).
func TestRunActivityNowBurstTriggersAdopt(t *testing.T) {
	fastActivity(t)
	stub := &activityBurstStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 5})

	// Сначала помечаем, что сегодня взятие уже пробовали (имитируем skip путешествия в 09:00), проверяем, что после отчёта дебаунс снят и пропускает.
	s.markAdoptTried("u1")
	s.RunActivityNow()

	if n := len(stub.reportBodies); n != 5 {
		t.Errorf("report bodies=%d want 5", n)
	}
	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1 (после отчёта проверяем наличие питомца)", n)
	}
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("buddy/first calls=%d want 1 (после добора объёма серией из 5 должны повторить взятие)", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("buddy/agreement calls=%d want 1", n)
	}
}

// TestRunActivityNowBurstSkipsAdoptWhenBuddyExists: номер с питомцем после отчёта взятие не триггерит.
func TestRunActivityNowBurstSkipsAdoptWhenBuddyExists(t *testing.T) {
	fastActivity(t)
	var reportCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			reportCalls.Add(1)
			w.Write([]byte(`{"code":0}`))
		case "/activity/growth/buddy/info":
			// Питомец уже есть
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":7,"name":"档案喵"}}}`)) // данные: имя питомца-фикстуры (значение name), не переводим
		case "/activity/growth/buddy/first":
			t.Errorf("номер с питомцем не должен триггерить взятие")
		case "/activity/growth/streak":
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 5})

	s.RunActivityNow()
	if n := reportCalls.Load(); n != 5 {
		t.Errorf("report calls=%d want 5", n)
	}
}

// TestRunActivityNowBurstCountDefault1: по умолчанию ActivityReportCount=1 отчёт (совместимость со старым).
func TestRunActivityNowBurstCountDefault1(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up}) // ActivityReportCount по умолчанию — New откатывает к 1

	s.RunActivityNow()
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("report calls=%d want 1 (дефолт=1, совместимость со старым)", n)
	}
}

// TestRunActivityNowBurstBreakDoesNotSelfCheck: в серии из 5 провалился средний — остаток не шлём,
// streak-самопроверку не гоняем, не берём (ok==0).
func TestRunActivityNowBurstBreakDoesNotSelfCheck(t *testing.T) {
	fastActivity(t)
	var reportCalls, streakHits, firstCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			n := reportCalls.Add(1)
			if n == 3 { // 3-й провалился
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			w.Write([]byte(`{"code":0}`))
		case "/activity/growth/streak":
			streakHits.Add(1)
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":3}}}`))
		case "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case "/activity/growth/buddy/first":
			firstCalls.Add(1)
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 5})

	s.RunActivityNow()
	if n := reportCalls.Load(); n != 3 { // после провала 3-го — break, дальше не шлём
		t.Errorf("report calls=%d want 3 (после провала 3-го — break)", n)
	}
	if streakHits.Load() != 0 {
		t.Errorf("streak hits=%d want 0 (отчёт не полон — не шлём)", streakHits.Load())
	}
	if firstCalls.Load() != 0 {
		t.Errorf("first calls=%d want 0 (отчёт не полон — не берём)", firstCalls.Load())
	}
}

// TestRunCheckinDoesNotTriggerTravel: хвост подписания путешествия больше не гоняет (путешествия выделены в независимый план).
func TestRunCheckinDoesNotTriggerTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunCheckinNow()

	// Подписание больше не тянет за собой путешествия: buddy/info не должен вызываться.
	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0 (путешествия отделены от подписания)", n)
	}
}

// TestNextWakeTravelIndependent: у путешествий независимая точка, с подписанием не влияют друг на друга.
func TestNextWakeTravelIndependent(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{21},
		TravelHours:    []int{9},
		ActivityHours:  []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v (независимая точка путешествий 09:00)", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskTravel {
		t.Errorf("kinds=%v want [travel]", kinds)
	}
}

// TestNextWakeActivityIndependent: у отчёта активности независимая точка.
func TestNextWakeActivityIndependent(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{21},
		TravelHours:    []int{9},
		ActivityHours:  []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 9, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 10, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v (независимая точка активности 10:00)", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskActivity {
		t.Errorf("kinds=%v want [activity]", kinds)
	}
}

// TestNextWakeTravelDisabled: после отключения путешествий точек путешествий в плане нет (подписание как было).
func TestNextWakeTravelDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{9, 21},
		TravelHours:    []int{9},
		TravelDisabled: true,
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	// Путешествия отключены — точки 09:00 путешествий быть не должно, ближайшая — подписание 09:00 (тот же час, но подписание не отключено).
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) {
		t.Errorf("kinds=%v want содержит checkin", kinds)
	}
	if hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v не должен содержать travel (отключён)", kinds)
	}
}

// TestNextWakeActivityDisabled: после отключения отчёта активности точек активности в плане нет.
func TestNextWakeActivityDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:     []int{9, 21},
		ActivityHours:    []int{10},
		ActivityDisabled: true,
		KeepaliveHours:   []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 9, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v (активность отключена — пропускаем 10:00)", at, want)
	}
	if hasKind(kinds, taskActivity) {
		t.Errorf("kinds=%v не должен содержать activity (отключён)", kinds)
	}
}

// TestCheckinDisabledTravelStillRuns: при отключённом подписании путешествия/активность идут как были (критерий приёмки 2).
func TestCheckinDisabledTravelStillRuns(t *testing.T) {
	s := New(Config{
		CheckinHours:    []int{9, 21},
		CheckinDisabled: true,
		TravelHours:     []int{9},
		ActivityHours:   []int{10},
		KeepaliveHours:  []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v (подписание отключено, путешествия 09:00 идут как были)", at, want)
	}
	if hasKind(kinds, taskCheckin) {
		t.Errorf("kinds=%v не должен содержать checkin (отключён)", kinds)
	}
	if !hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v должен содержать travel (подписание отключено, но путешествия независимы)", kinds)
	}
}

// TestAllFourDisabledNoSpin: все четыре класса отключены — Run не крутится вхолостую.
func TestAllFourDisabledNoSpin(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CheckinHours:      []int{9, 21},
		TravelHours:       []int{9},
		ActivityHours:     []int{10},
		KeepaliveHours:    []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil (все четыре отключены)", at, kinds)
	}
}

// TestNextWakeSameHourTravelAndCheckin: путешествия и подписание на одном часе — выполняем оба класса задач.
func TestNextWakeSameHourTravelAndCheckin(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{9, 21},
		TravelHours:    []int{9},
		ActivityHours:  []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v want содержит checkin+travel (две задачи на 09:00)", kinds)
	}
}

// TestRunDispatchesActivityAndTravel: Run по времени раздаёт activity и travel (вверх по-настоящему не бьём, пул пуст).
func TestRunDispatchesActivityAndTravel(t *testing.T) {
	fastActivity(t)
	fastTravel(t)
	// Пустой pool — RunActivityNow/RunTravelNow обходят 0 номеров и возвращаются, не блокируясь.
	p := pool.New("")
	up := &upstream.Client{}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{},
		TravelHours:    []int{},
		ActivityHours:  []int{},
		KeepaliveHours: []int{},
	})
	// Все четыре hours пусты — nextWake откатывается к дефолту — строим timer, по отмене ctx возвращаемся.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run не вернулся после отмены ctx")
	}
}
