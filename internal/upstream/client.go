// Package upstream инкапсулирует все HTTP-вызовы к апстриму CodeBuddy (chat / billing / auth),
// а также классификацию ошибок (движет автомат кулдаунов pool).
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// ErrKind — классификация ошибок, по ней pool определяет длительность кулдауна.
type ErrKind int

const (
	ErrNone           ErrKind = iota // успех
	ErrHardCredit                    // нехватка баланса (402 или ключевое слово в body) → длинный кулдаун
	ErrSoftRate                      // мягкий лимит 429 → короткий кулдаун
	ErrSessionDead                   // 401 + 12153 протухшая offline-сессия → отключение
	ErrNotFound                      // эпизодический 404 апстрима → короткий кулдаун, счёт ошибок не копим (против лавины)
	ErrServer                        // сбой апстрима 5xx
	ErrContentBlocked                // перехват контент-политикой (400 + текст модерации) → аккаунт не штрафуем, идём в деградированный ретрай
	ErrBadParams                     // тело запроса не разобралось (400 + Unmarshal chat params failed / 11101) → аккаунт не штрафуем, но ротируем
	ErrClient                        // прочие 4xx / бизнес-ошибки
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error — ошибка апстрима с классификацией.
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers — ключевые слова нехватки баланса (двойной канал: сравнение в нижнем регистре + сравнение китайского оригинала). // Значения — логика, не менять.
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softRateMarkers — ключевые слова лимита/дросселирования (двойной канал: сравнение в нижнем регистре + сравнение китайского оригинала). // Значения — логика, не менять.
// Апстрим возвращает семантику лимита и при статусе не 429 (например, 200 + code 11140
// "The model provider is rate-limiting requests.", 400 + "rate limit"),
// Если такие ответы не распознавать, аккаунт ни охлаждается, ни кормит прерыватель, и следующий запрос снова его выберет (issue #28).
//
// Словарь матчится по подстроке, лучше недобрать, чем перебрать: только формулировки, явно указывающие на «дросселирование скорости запросов/использования модели».
// Дефисные формы (rate-limiting / rate-limited) нужны отдельными строками — Contains через '-' не перепрыгивает.
// "too many" заденет и клиентские ошибки параметров вроде "too many tokens", цена — мягкий кулдаун номера
// на один SoftCooldown (по умолчанию 60s) с самовосстановлением, куда меньше цены пропущенного лимита с повторным выбором того же номера.
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded (дросселирование использования, не биллинговый остаток)
	"请求过于频繁", "限流",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// contentBlockedMarkers — ключевые слова перехвата контент-политикой (матч подстроки без учёта регистра). // Значения — логика, не менять.
//
// Позиция: апстрим модерирует по точному посимвольному отпечатку, шаблонные фразы из system (например,
// внедрённые инструкции Claude Code/Codex) вызывают HTTP 400 + текст ниже. Это «ложное срабатывание» (легитимный трафик убит модерацией),
// не проблема аккаунта — баланс здоров, лимита нет, сессия жива, поэтому ErrContentBlocked
// в applyErrorPolicy аккаунт не штрафует (без кулдауна/прерывания/NoteError), вместо этого шлюз идёт в деградированный ретрай.
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// badParamsMarkers — ключевые слова ошибки разбора тела запроса (смежно с issue #41): HTTP 400 + апстрим
// "Unmarshal chat params failed..." (code 11101). Это «проблема в body, отправленном апстриму»,
// к здоровью аккаунта отношения не имеет — номер не штрафуем, но всё равно ротируем (commit B).
var badParamsMarkerMsg = "Unmarshal chat params failed"
var badParamsMarkerCode = `"code":11101`

// alreadyCheckinMarkers — ключевые слова «сегодня уже отмечен» (апстрим на повторный чекин отвечает code!=0,
// замерено code=10001/14001 «сегодня уже отмечен»). Матчим вхождением только по *Error.Msg,
// ошибки сетевого/разборного слоя здесь не распознаём (см. IsAlreadyCheckin). // Значения — логика, не менять.
var alreadyCheckinMarkers = []string{"已签到", "already"}

// softRateResetLoc: время сброса в тексте 429 6004 апстрима всегда трактуем как UTC+8 (так пишет сам апстрим,
// от часового пояса контейнера не зависит).
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc раскрывает фиксированный часовой пояс времени сброса (чтобы тесты строили/проверяли в той же мере).
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode — бизнес-code, явно указывающий на «лимит 429 уровня модели».
// Апстрим им выражает «превышено использование этой модели» (code 6004, msg с «сброс будет в …»),
// а не лимит аккаунта целиком — аккаунт здоров, ограничена только эта модель сейчас (issue #31).
const modelRateLimitCode = "6004"

// softRateResetRe матчит «сброс будет в …», захватывая строку времени посередине. // Значение — логика, не менять.
const softRateResetRe = `将在 (.+?) 重置`

// softRateTimeLayout — формат времени сброса апстрима (без суффикса пояса; пояс фиксирован UTC+8).
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit сообщает, указывает ли body 429 явно на лимит уровня модели (бизнес-code 6004).
// Нужно, чтобы различить «мягкий лимит уровня аккаунта» (охлаждаем аккаунт) и «лимит использования уровня модели» (достаточно сменить модель).
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `"code": 6004` / `"code":"6004"` — все попадают (терпимость к пробелам в JSON).
	re := regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	return re.MatchString(body)
}

// ParseSoftRateReset разбирает из body 429 время «сброса в …» (текст апстрима в UTC+8).
// Успех возвращает разобранный **момент настенных часов** (трактуем как UTC+8), неудача — ноль + false.
// Внутри сначала IsModelRateLimit: не модельный лимит (не 6004) не возвращаем, даже если слово «сброс» есть — у такого сброса
// нет кулдаун-семантики (как общее лимит-подсказка 11140), разбор ошибочно сузил бы кулдаун.
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	re := regexp.MustCompile(softRateResetRe)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // Убираем суффикс, трактуем фиксированно по softRateResetLoc
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify определяет класс ошибки по HTTP-статусу + body.
//
// Порядок проверок — от «строгого» к «мягкому», у очерёдности каждого слоя есть семантическое основание:
//  1. 402 / hardMarkers — исчерпание биллингового лимита, самое строгое и наименее самовосстанавливающееся, проверяем первым.
//     Семантика "quota exceeded" лежит на границе биллинга/лимита, исторически отнесена к hard_credit, в этот раз сохраняем
//     (риск обратного mis-класса уже записан в issue #28, ждём подтверждения по сырому ответу апстрима).
//  2. sessionDeadMarkers — терминальное состояние, требующее ручного перелогина. Если body 401 содержит и "12153", и
//     "rate limit" (например, смешанная страница ошибки шлюза), относим к session_dead: короткий кулдаун мёртвую сессию не воскресит,
//     mis-класс в лимит оставил бы мёртвый номер выбираться в пуле снова и снова; к тому же маркеры слоя — точные слова (12153 и т.п.),
//     конкретнее широких подстрок слоя лимита, конкретное важнее широкого.
//  3. softRateMarkers — текст лимита при статусе не 429 (точка исправления issue #28).
//     Позиция покрывает статусы 200/400/403/5xx; 429 с текстом в body закорачивается здесь,
//     результат тот же soft_rate, согласуется со следующим слоем.
//  4. status==429 — страховое распознавание, когда в body текста нет.
//  5. 404 / 5xx / прочие 4xx — обычная классификация вне лимита.
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// Перехват контент-политикой (HTTP 400 + текст модерации): проверяем раньше общего ErrClient.
	// Это сигнал ложного срабатывания, аккаунт не штрафуем, обрабатывает шлюз деградированным ретраем (см. handler.applyErrorPolicy).
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// Ошибка разбора тела запроса (HTTP 400 + Unmarshal chat params failed / code 11101):
		// это «проблема в body, отправленном апстриму». Усечение на стороне шлюза уже убито через 413 (issue #41 commit A),
		// оставшийся источник — кривой JSON самого клиента: со сменой аккаунта всё равно будет 400, штрафовать номер нельзя (зря охладим хороший номер).
		// Относим к ErrBadParams: без кулдауна/прерывания/учёта ошибки, но **всё равно ротируем** (у разных аккаунтов могут быть разные
		// права на модели, стоит попробовать ещё раз).
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		return ErrClient
	}
	// Случай HTTP 200 с бизнес-code не 0 и ключевым словом баланса уже пойман выше hardMarkers.
	return ErrNone
}

// apiEnvelope — единый конверт апстрима.
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client — HTTP-клиент апстрима. Поля Base можно перекрывать для удобства тестов.
type Client struct {
	HTTP *http.Client

	// ChatHTTP — выделенный client для chat SSE: без общего лимита длительности (Timeout=0), первый байт ограничивает
	// Transport.ResponseHeaderTimeout, простой в потоке — IdleTimeout.
	// Делит один экземпляр *http.Transport с HTTP, пул соединений не дублируется.
	ChatHTTP *http.Client

	// HeaderTimeout — таймаут до первого байта chat SSE (заголовки ответа); <=0 означает не задан (откат к HTTP.Timeout).
	HeaderTimeout time.Duration
	// IdleTimeout — таймаут простоя в потоке chat SSE; <=0 означает выключенный мониторинг простоя.
	IdleTimeout time.Duration

	// effortsMu/efforts — кэш supportedEfforts по моделям (обновляет FetchModels) для даунгрейда effort в теле запроса.
	effortsMu sync.RWMutex
	efforts   map[string][]string

	// SanitizeFingerprints — выключатель десенсибилизации отпечатков чёрного списка в исходящем теле (по умолчанию true; false полностью возвращает как было).
	SanitizeFingerprints bool

	// UserAgent — перекрытие исходящего User-Agent (пусто = текущий clientUA).
	// Действует на все исходящие запросы: chat / refresh / checkin / balance (включая report/travel) / FetchModels.
	// Раскопки issue #42: столбец «используемый клиент» на сайте атрибутируется сервером по UA/X-Product исходящих запросов,
	// официальный десктопный UA WorkBuddy — `WorkBuddy/<version>` (product.json applicationName=WorkBuddy,
	// UserAgentHttpInterceptor склеивает префикс productName/platform в UA). По умолчанию сохраняем как было
	// (соображения очистки отпечатков), переписываем, только если пользователь явно настроил.
	UserAgent string

	ChatBaseCN    string
	BillingBaseCN string
}

// New — производственные значения по умолчанию. Настраиваем пул соединений, чтобы меньше TLS-хендшейков.
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		// Жёсткий верхний предел до первого байта chat SSE (на короткие RPC фактически не влияет: их общий лимит 120s истекает раньше).
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:                 &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:             &http.Client{Timeout: 0, Transport: tr}, // без общего лимита; первый байт ведёт ResponseHeaderTimeout
		SanitizeFingerprints: true,
		ChatBaseCN:           "https://copilot.tencent.com",
		BillingBaseCN:        "https://www.codebuddy.cn",
	}
}

// chatHTTP возвращает выделенный client для чата; если не задан (например, тесты внедрили только HTTP) — откат к HTTP.
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

func (c *Client) chatBase(a *auth.Auth) string {
	return c.ChatBaseCN
}

// prepareBody собирает исходящее тело запроса (выключатель десенсибилизации — Client.SanitizeFingerprints).
func (c *Client) prepareBody(body []byte) []byte {
	return PrepareBodyOptWithEfforts(body, c.SanitizeFingerprints, c.effortsSnapshot())
}

// effortsSnapshot возвращает копию кэша возможностей effort; nil означает неизвестно (пропускаем без даунгрейда).
func (c *Client) effortsSnapshot() map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	if len(c.efforts) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(c.efforts))
	for k, v := range c.efforts {
		cp[k] = v
	}
	return cp
}

func (c *Client) billingBase(a *auth.Auth) string {
	return c.BillingBaseCN
}

// Пути эндпоинтов billing-домена (billingBase + path). balance/checkin и report (report.go) — тот же домен,
// все запросы идут единообразно через billingJSON.
const (
	billingMeterPath = "/v2/billing/meter/get-user-resource"
	dailyCheckinPath = "/v2/billing/meter/daily-checkin"
)

// doJSON отправляет запрос и разбирает конверт; при HTTP не 2xx или бизнес-code != 0 возвращает *Error с фрагментом body.
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken обновляет access token; при успехе обновляет поля a (пропущенные значения сохраняют старые),
// вызывающий код отвечает за SaveAtomic. Всё время держим замок a, чтобы конкурентный SaveAtomic не прочитал наполовину обновлённый token.
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	c.RefreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry: если в ответе нет expiresIn — сохраняем старое время истечения, чтобы не устроить шторм обновлений.
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream отправляет chat-запрос и возвращает сырой SSE-поток body (вызывающий код отвечает за Close).
// При не 2xx: rc равен nil, body — тело ответа апстрима (вызывающий код скормит в Classify(status, string(body))), err равен nil;
// err возвращаем только при ошибке транспортного слоя.
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(c.prepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] chat_stream uid=%s: ошибка транспорта: %v", logfmt.UID8(a.UID), err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] chat_stream uid=%s: upstream %d %s body=%s",
			logfmt.UID8(a.UID), resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// Ветка успеха: владение cancel отдаём monitorBody (его Close вызовет cancel);
	// при IdleTimeout<=0 monitorBody возвращает нижний поток как есть и cancel никто не дёргает — приемлемо:
	// у ctx нет deadline и goroutine, соединение штатно чистит resp.Body.Close.
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ModelInfo — динамическая информация о модели (включая maxInputTokens/maxOutputTokens).
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts (пусто = неизвестно/фиксированная ступень)
}

// FetchModels дёргает динамический интерфейс моделей апстрима.
// Имена полей выровнены с фактическим ответом апстрима: maxInputTokens (не contextWindow), maxOutputTokens (не maxTokens).
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // Переиспользуем общие заголовки (Origin/Referer/UA/Accept/Content-Type)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]struct {
		ID              string
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
		Efforts         []string
	}, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = struct {
			ID              string
			Name            string
			MaxInputTokens  int64
			MaxOutputTokens int64
			Disabled        bool
			Efforts         []string
		}{m.ID, m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled, m.Reasoning.SupportedEfforts}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Efforts,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// Обновляем кэш возможностей effort (для даунгрейда тела запроса; модели без supportedEfforts в кэш не входят).
	cache := make(map[string][]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
	}
	c.effortsMu.Lock()
	c.efforts = cache
	c.effortsMu.Unlock()
	return out, nil
}

// UserResource опрашивает текущий расходуемый баланс баллов аккаунта (агрегация CycleCapacity по всем пакетам, отрицательные прижимаем к 0).
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingJSON(a, http.MethodPost, billingMeterPath, body)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		var r int64
		switch {
		case acct.CycleCapacitySize > 0:
			r = acct.CycleCapacityRemain
		case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
			r = acct.CycleCapacityRemain
		default:
			r = acct.CapacityRemain
		}
		if r < 0 {
			r = 0
		}
		remain += r
	}
	return remain, nil
}

// DailyCheckin выполняет ежедневный чекин. Уже отмеченный (бизнес-code не 0) тоже возвращает ошибку, вызывающий код различает по msg.
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.billingJSON(a, http.MethodPost, dailyCheckinPath, map[string]any{})
	return err
}

// IsAlreadyCheckin сообщает, означает ли err «сегодня уже отмечен» (апстрим идемпотентно отклоняет повторный чекин).
// Признаём только классифицированный *Error (бизнес-code или HTTP-ошибка): ошибки сетевого/разборного слоя нельзя считать идемпотентным успехом,
// иначе джиттер при догоняющем чекине после простоя ложно запишется как already, а аккаунт в этот день фактически не отметится, но будет признан нормальным.
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
