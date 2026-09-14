package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestGrowthStreakParsesDays разбирает data.streak.days (тот же срез, что в probe_active.py).
func TestGrowthStreakParsesDays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activity/growth/streak" {
			t.Errorf("path=%s want /activity/growth/streak", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method=%s want GET", r.Method)
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"streak":{"days":3,"total_rewards":1}}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	days, err := c.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("GrowthStreak: %v", err)
	}
	if days != 3 {
		t.Errorf("days=%d want 3", days)
	}
}

// TestGrowthStreakDefaultZero — без полей streak/days → 0 (days==0 и есть сигнальный флаг самопроверки).
func TestGrowthStreakDefaultZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	days, err := c.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("GrowthStreak: %v", err)
	}
	if days != 0 {
		t.Errorf("days=%d, хотим 0 (нулевое значение при отсутствующих полях)", days)
	}
}

// TestGrowthStreakServerError — HTTP не-2xx → *Error.
func TestGrowthStreakServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("want error on 500")
	}
}

// TestGrowthStreakBusinessCode — бизнес-code != 0 → ошибка (тихие провалы помимо не-2xx тоже обязаны наблюдаться).
func TestGrowthStreakBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"code":12000,"msg":"internal"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("want error on non-zero business code")
	}
}
