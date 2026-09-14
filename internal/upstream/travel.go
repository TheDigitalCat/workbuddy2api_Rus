// travel.go — интерфейсы «путешествия котёнка» growth-домена: опрос статуса / отправка / получение награды / приют / соглашение.
// Всё идёт через chatBase (copilot.tencent.com, без префикса /v2) + BillingHeaders, конверт как у doJSON.
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

// Пути growth-домена (замерены).
const (
	travelStatusPath   = "/activity/growth/buddy/travel/status"
	travelDepartPath   = "/activity/growth/buddy/travel/depart"
	travelClaimPath    = "/activity/growth/buddy/travel/claim"
	buddyInfoPath      = "/activity/growth/buddy/info"
	buddyFirstPath     = "/activity/growth/buddy/first"
	buddyAgreementPath = "/activity/growth/buddy/agreement"
	streakPath         = "/activity/growth/streak"
)

// buddyTaskIncompleteMarker — ключевое слово бизнес-ошибки «порог приюта не выполнен» (всплывает при HTTP 400).
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// Buddy — текущее досье кота аккаунта; nil (data.buddy равен null) означает отсутствие кота.
type Buddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// TravelState — состояние путешествия котёнка.
type TravelState struct {
	State             string `json:"state"`               // idle / traveling / arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // сегодня уже отправляли (сброс в 00:00 CST календарных суток)
	RecordID          int64  `json:"record_id"`           // id записи в пути/по прибытии, для claim обязателен
	RewardCredit      int64  `json:"reward_credit"`       // причитающиеся по прибытии призовые баллы
}

// growthJSON шлёт запрос growth-домена и разбирает конверт; при body nil тело запроса не несём.
// Семантика ошибок как у doJSON: HTTP не-2xx / бизнес-code != 0 → *Error.
func (c *Client) growthJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	return c.doJSON(req)
}

// TravelStatus опрашивает состояние путешествия котёнка.
func (c *Client) TravelStatus(a *auth.Auth) (*TravelState, error) {
	data, err := c.growthJSON(a, http.MethodGet, travelStatusPath, nil)
	if err != nil {
		return nil, err
	}
	var st TravelState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TravelDepart отправляет кота в путешествие; locationID по замерам 1~4 (диапазоны награды/длительности одинаковы).
func (c *Client) TravelDepart(a *auth.Auth, locationID int) error {
	_, err := c.growthJSON(a, http.MethodPost, travelDepartPath, map[string]any{"location_id": locationID})
	return err
}

// TravelClaim забирает награду по прибытии, возвращает reward_credit.
func (c *Client) TravelClaim(a *auth.Auth, recordID int64) (int64, error) {
	data, err := c.growthJSON(a, http.MethodPost, travelClaimPath, map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		// Отсутствие поля награды провалом не считаем: вызывающая сторона просто залогирует 0.
		_ = json.Unmarshal(data, &resp)
	}
	return resp.RewardCredit, nil
}

// BuddyInfo опрашивает текущее досье кота; (nil, nil) означает отсутствие кота (data.buddy равен null).
func (c *Client) BuddyInfo(a *auth.Auth) (*Buddy, error) {
	data, err := c.growthJSON(a, http.MethodGet, buddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	// null / отсутствующее поле / пустой объект — всё трактуем как отсутствие кота.
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var b Buddy
	if err := json.Unmarshal(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BuddyFirst приютит первого кота. Без кота и при пройденном пороге conversation начисляет 300 баллов.
// При невыполненном пороге возвращает HTTP 400 (см. IsBuddyTaskIncomplete), это ожидаемое поведение, вызывающая сторона молча пропускает.
func (c *Client) BuddyFirst(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyFirstPath, map[string]any{})
	return err
}

// BuddyAgreement принимает соглашение (идемпотентно, повторные вызовы без побочных эффектов).
func (c *Client) BuddyAgreement(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true})
	return err
}

// GrowthStreak опрашивает число дней входа подряд (только чтение, oracle). Ответ `data.streak.days` (probe_active.py
// замеренный срез: `(s.get("data", {}).get("streak", {}) or {}).get("days")`).
// При неудаче GET (HTTP не-2xx / бизнес-code != 0) возвращает *Error; при отсутствующих полях streak/days возвращает 0
// (days==0 — это сигнальный флаг самопроверки активности «отчёт 200, но молча отброшен»).
func (c *Client) GrowthStreak(a *auth.Auth) (int, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Streak.Days, nil
}

// IsBuddyTaskIncomplete определяет «порог приюта не выполнен»: HTTP 400 + ключевое слово first_buddy.
// Такую ошибку в тот же день не повторяем (чтобы не бомбардировать апстрим ретраями).
func IsBuddyTaskIncomplete(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ue.Msg), buddyTaskIncompleteMarker)
}
