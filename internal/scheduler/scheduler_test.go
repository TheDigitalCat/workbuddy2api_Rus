package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// TestNextWakeKeepaliveOnly: подписание уже прошло — будим по часу keepalive.
func TestNextWakeKeepaliveOnly(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeSameInstantFiresAll: подписание и keepalive на одном часе — выполняем оба класса задач.
func TestNextWakeSameInstantFiresAll(t *testing.T) {
	s := New(Config{
		CheckinHours:     []int{9, 22},
		TravelHours:      []int{}, // глушим точки путешествий (меряем только подписание+keepalive на одном часе)
		ActivityHours:    []int{}, // глушим точки активности
		KeepaliveHours:   []int{22},
		TravelDisabled:   true,
		ActivityDisabled: true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 21, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskKeepalive) {
		t.Errorf("kinds=%v want checkin+keepalive (две задачи на один момент)", kinds)
	}

	// После 22:00 следующее — 09:00 следующего дня, только подписание (путешествия/активность отключены).
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 22, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeNothingScheduled: оба класса задач пусты — возвращаем zero, Run только ждёт сигнал выхода.
func TestNextWakeNothingScheduled(t *testing.T) {
	s := &Scheduler{cfg: Config{}}
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestNextWakeCheckinDisabled: после явного отключения подписания точек подписания в расписании нет (keepalive как был).
func TestNextWakeCheckinDisabled(t *testing.T) {
	s := New(Config{CheckinDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v (подписания на 21:00 больше не должно быть)", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeKeepaliveDisabled: после явного отключения keepalive точек keepalive в расписании нет (подписание как было).
func TestNextWakeKeepaliveDisabled(t *testing.T) {
	s := New(Config{KeepaliveDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v (keepalive на 22:00 больше не должно быть)", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeBothDisabledNothingScheduled: все четыре класса явно отключены — будить нечего.
func TestNextWakeBothDisabledNothingScheduled(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CheckinHours:      []int{9, 21},
		KeepaliveHours:    []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestRunAllDisabledNoSpinNoCalls: все четыре класса отключены — Run не крутится вхолостую (только ждёт сигнал выхода),
// и не должен триггерить ни одного запроса вверх.
func TestRunAllDisabledNoSpinNoCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:              p,
		Upstream:          up,
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Run(ctx) // блокируемся до отмены ctx (ждать нечего, timer не строим)
	elapsed := time.Since(start)

	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0 (все четыре отключены)", calls.Load())
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("Run returned after %v, before ctx done (не должен возвращаться раньше)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v (не должен крутиться/ждать в busy loop)", elapsed)
	}
}

func hasKind(kinds []taskKind, k taskKind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

// fakeUpstream одновременно имитирует billing и refresh.
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足") // данные: причина-цитата апстрима, не переводим

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// P0-1: 12153 отключает только после N подряд. Первые 2 провала обновления не должны убивать номер (защита от ложных).
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("1-й 12153 не должен отключать: %+v", st)
	}
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("2-й 12153 не должен отключать: %+v", st)
	}
	// 3-й подряд 12153 — отключаем.
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("3-й подряд 12153 должен отключать: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
}

// TestRunKeepaliveSessionDeadResetBySuccess: дважды 12153, затем успешное обновление — счёт обнуляется,
// следующие 12153 считаем заново с 1-го (исторические провалы больше не преследуют).
func TestRunKeepaliveSessionDeadResetBySuccess(t *testing.T) {
	var fails atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.Add(1) == 3 { // 3-й раз (второй круг этого цикла планировщика) — обновление успешно
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow() // 12153 #1
	s.RunKeepaliveNow() // 12153 #2
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("precondition: первые 2 не должны отключать: %+v", st)
	}
	s.RunKeepaliveNow() // обновление успешно — чистим счёт
	// Дальше 2 подряд 12153: считаем заново с нового счёта, всё ещё не должны отключать (исторический счёт сброшен).
	s.RunKeepaliveNow() // 12153 #1 (новый счёт)
	s.RunKeepaliveNow() // 12153 #2 (новый счёт)
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("после сброса счёта успешным обновлением 2 подряд 12153 не должны отключать: %+v", st)
	}
	s.RunKeepaliveNow() // 12153 #3 (новый счёт) — отключаем
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("3-й 12153 нового счёта должен отключать: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// не должен паниковать
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	_ = errors.New("unused")
}

// checkinStub — конфигурируемый апстрим подписания: управляем ответом подписания (ok/already/fail), провалом обновления,
// возвратом баланса. Попадания по веткам считаем через atomic — удобно для конкурентно-безопасных assert'ов.
type checkinStub struct {
	checkinBody    string // полный body ответа /daily-checkin (включая code/msg)
	checkinStatus  int    // HTTP-статус /daily-checkin (0=200)
	refreshFail    bool   // падает ли /token/refresh (возвращает 12153 session dead)
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (s *checkinStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			if s.checkinStatus != 0 {
				w.WriteHeader(s.checkinStatus)
			}
			w.Write([]byte(s.checkinBody))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(s.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			s.refreshCalls.Add(1)
			if s.refreshFail {
				w.WriteHeader(401)
				w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

// newCheckinS собирает Pool+Upstream+Scheduler, token номера не протух (предобновление не триггерим).
func newCheckinS(t *testing.T, stub *checkinStub) (*Scheduler, *pool.Pool) {
	t.Helper()
	srv := stub.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestCheckinAllOK: подписание успешно — ok, баланс подтянут, указатель credits непуст.
func TestCheckinAllOK(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":0,"msg":"ok","data":{}}`,
		resourceRemain: 500,
	})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].Status != CheckinOK {
		t.Fatalf("out=%+v want ok", out)
	}
	if out[0].Credits == nil || *out[0].Credits != 500 {
		t.Errorf("credits=%v want 500", out[0].Credits)
	}
	if st, _ := p.Status("u1"); st.Credits != 500 {
		t.Errorf("pool credits=%d want 500", st.Credits)
	}
}

// TestCheckinAllAlready: цитату апстрима «今天已签到» (данные, не переводим) пишем как already, тело 400 в detail не подтягиваем.
func TestCheckinAllAlready(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `{"code":14001,"msg":"今天已签到"}`, // данные: входной body апстрима (цитата «今天已签到»), не переводим
		resourceRemain: 300,
	})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out[0].Status != CheckinAlready {
		t.Errorf("status=%q want already", out[0].Status)
	}
	if out[0].Detail != "" {
		t.Errorf("подписанный не должен подтягивать detail, got=%q", out[0].Detail)
	}
}

// TestCheckinAllFail: апстрим подписания 500 — fail, в detail пишем ошибку.
func TestCheckinAllFail(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{
		checkinBody:    `boom`,
		checkinStatus:  500,
		resourceRemain: 300,
	})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinFail {
		t.Errorf("status=%q want fail", out[0].Status)
	}
	if out[0].Detail == "" {
		t.Error("fail должен заполнить detail")
	}
}

// TestCheckinAllSkipsDisabled: отключённый номер пишем skipped, в подписании не участвует.
func TestCheckinAllSkipsDisabled(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("dis", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	m := map[string]CheckinStatus{}
	for _, o := range out {
		m[o.UID] = o.Status
	}
	if m["dis"] != CheckinSkipped {
		t.Errorf("dis=%q want skipped", m["dis"])
	}
	if m["ok"] != CheckinOK {
		t.Errorf("ok=%q want ok", m["ok"])
	}
}

// TestCheckinAllSkipsNoCredentials: номер без refreshToken пишем skipped(no credentials).
func TestCheckinAllSkipsNoCredentials(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "notoken", AccessToken: "", RefreshToken: "", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinSkipped {
		t.Errorf("status=%q want skipped", out[0].Status)
	}
	if out[0].Detail != "no credentials" {
		t.Errorf("detail=%q want no credentials", out[0].Detail)
	}
}

// TestCheckinAllRefreshBeforeExpiry: token скоро протухает — перед подписанием обновляем, после успеха продолжаем подписание.
func TestCheckinAllRefreshBeforeExpiry(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// ExpiresAt протухает через 5 минут, попадает в окно checkinRefreshSkew(10min) — триггерим предобновление.
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if stub.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d want 1 (окно протухания должно предобновить)", stub.refreshCalls.Load())
	}
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok (после успешного обновления продолжаем подписание)", out[0].Status)
	}
	if a.AccessToken != "new" {
		t.Errorf("token не обновлён: %s", a.AccessToken)
	}
}

// TestCheckinAllRefreshFlakyContinues: обновление дёрнулось с провалом, но token по-настоящему не протух — продолжаем подписание (не блокируем).
func TestCheckinAllRefreshFlakyContinues(t *testing.T) {
	stub := &checkinStub{
		checkinBody:    `{"code":0,"msg":"ok","data":{}}`,
		refreshFail:    true,
		resourceRemain: 100,
	}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// token протухает через 5 минут (в окне — пробуем обновить), но NeedsRefresh(0) всё ещё false (по-настоящему не протух).
	// Обновление вернуло 12153, но это не терминальный session dead — token всё ещё валиден, продолжаем подписание.
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok (дёрнувшееся обновление не должно блокировать подписание)", out[0].Status)
	}
	if stub.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d want 1", stub.refreshCalls.Load())
	}
}

// TestCheckinAllRefreshTrulyExpiredFails: обновление провалилось и token по-настоящему протух — пишем fail.
func TestCheckinAllRefreshTrulyExpiredFails(t *testing.T) {
	stub := &checkinStub{
		checkinBody: `{"code":0,"msg":"ok","data":{}}`,
		refreshFail: true,
	}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	// ExpiresAt уже в прошлом — NeedsRefresh(0) true (по-настоящему протух), провал обновления — это fail.
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, _ := s.CheckinAll()
	if out[0].Status != CheckinFail {
		t.Errorf("status=%q want fail (по-настоящему протух + провал обновления)", out[0].Status)
	}
	if out[0].Detail == "" || !strings.HasPrefix(out[0].Detail, "refresh:") {
		t.Errorf("detail=%q want префикс refresh:", out[0].Detail)
	}
}

// TestCheckinAllBusy: конкурентный второй вызов возвращает ErrBusy (сериализация через TryLock).
func TestCheckinAllBusy(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	s, _ := newCheckinS(t, stub)
	// Вручную держим лок — имитируем идущее подписание, повторный CheckinAll должен дать ErrBusy.
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()
	_, err := s.CheckinAll()
	if !errors.Is(err, ErrBusy) {
		t.Errorf("err=%v want ErrBusy", err)
	}
}

// TestCheckinAllReenablesCoolingAccount: охлаждённый номер успешно подписался + баланс восстановился — размораживаем.
func TestCheckinAllReenablesCoolingAccount(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 500}
	srv := stub.server()
	defer srv.Close()
	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足") // данные: причина-цитата апстрима, не переводим
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out[0].Status != CheckinOK {
		t.Errorf("status=%q want ok", out[0].Status)
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Errorf("подписание + восстановленный баланс должны разморозить: %+v", st)
	}
}
