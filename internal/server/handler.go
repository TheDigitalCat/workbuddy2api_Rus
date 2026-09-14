// Package server отдаёт OpenAI-совместимый HTTP-интерфейс, внутри — выбор номера из pool и проксирование в upstream.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config — зависимости handler.
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // пусто = без аутентификации
	MaxRotate int    // максимум смен номера на запрос, по умолчанию 3
	// MaxBodyBytes — верхний предел размера тела chat-запроса; <=0 означает запасные 8<<20 (8MB).
	// При превышении сразу 413 request_body_too_large (без тихой обрезки перед апстримом, issue #41).
	MaxBodyBytes int64
	// Session — роутер липких сессий (опционально; nil = без липкости, чистый Pick по кругу).
	Session *session.Router
	// StickyCount возвращает текущее число липких привязок (для /status); при nil отдаёт 0.
	StickyCount func() int
	// RedisMode — поле наблюдения ("upstash" / "noop"), отдаётся через /status.
	RedisMode    string
	SoftCooldown time.Duration // база мягкого охлаждения для 429/текстов лимита, по умолчанию 600s (при повторах — экспоненциальный бэкофф, потолок soft_rate_max)
	RefreshSkew  time.Duration // окно упреждающего обновления token, по умолчанию 10m

	// PromptMode "custom" (шлюз заменяет system собственным промптом) / "passthrough" (сквозная передача).
	PromptMode string
	// PromptText — текст системного промпта, подставляемый в режиме custom (из config.PromptText).
	PromptText string
}

// notFoundCooldown — фиксированная короткая длительность охлаждения при 404 апстрима.
// Причина отдельного контура от SoftCooldown: 404 — это **спорадическое** отсутствие пути на апстриме, а не «этот номер под лимитом»,
// при общем soft_rate (старт 600s + экспоненциальный рост) один случайный 404 наказал бы хороший номер на 10 минут с последующим удвоением.
// Поэтому достаточно фиксированных 60s от лавины: не зависит от soft_rate и не участвует в мягком бэкоффе.
const notFoundCooldown = 60 * time.Second

// ServiceName — идентификатор шлюза. Отдаётся одновременно в поле service ответа /healthz и в заголовке X-Service:
// когда хост (например workbuddy-switch, управляющий подпроцессом шлюза) опрашивает старый/чужой сервис на том же порту, тот даже
// при ответе 2xx не несёт наш идентификатор, и хост по нему распознаёт «ложный успех».
const ServiceName = "workbuddy2api"

// Handler — главный роутер.
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
}

// NewHandler создаёт handler.
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // база мягкого лимита (при повторах растёт экспоненциально)
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // по умолчанию custom: собственный промпт шлюза
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // запасной предел тела запроса 8MB
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "отсутствует или неверный API-ключ")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// Решение через ServableNow: при healthy>0, но полностью занятых in-flight chat даст 503, и проба обязана мерить тем же аршином,
	// иначе балансировщик будет лить трафик в экземпляр, который не может обслуживать.
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// Всегда без аутентификации (пробам балансировщика/оркестратора нужна лишь семантика 2xx/503), идентичность дублируется полем service и заголовком X-Service.
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": ServiceName,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// Статическая таблица моделей CN (api-reference §5, запасной вариант при отказе динамического интерфейса).
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// dynamicModelsCache — кэш динамических моделей.
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // время последнего успешного получения
	lastFail time.Time // время последней неудачи получения (негативный кэш)
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models возвращает список моделей: сначала динамический (кэш 1h), при неудаче — статическая таблица.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList динамически получает список моделей и упаковывает в формат OpenAI (с context_length).
func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":                mi.ID,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    mi.ContextWindow,
				"max_output_tokens": mi.MaxTokens,
			}
			if mi.ContextWindow == 0 {
				entry["context_length"] = 131072 // запасное значение
			}
			out = append(out, entry)
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels тянет список моделей через любой здоровый номер пула (с contextWindow/maxTokens), кэш 1h.
// При неудаче пишет метку в негативный кэш на 5min и в период охлаждения отдаёт статическую таблицу, не дёргая апстрим повторно.
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// Негативный кэш неудач: в период охлаждения апстрим больше не запрашиваем.
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// Неудача наказывает этот номер, чтобы следующий Pick не выбрал тот же упорно падающий; lastFail держит глобальный негативный кэш.
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // при успехе негативный кэш очищаем
	dynamicModelsCache.Unlock()
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// Предел тела запроса: LimitReader читает limit+1, чтобы обнаружить «превышение» (прочитали limit+1 байт — уже превышено),
	// при превышении сразу 413, не кормим апстрим обрезанным куском JSON (issue #41: обрезанный body роняет
	// unmarshal апстрима с unexpected EOF, а шлюз при этом зря наказывает номер и крутит ротацию).
	// 413 — клиентская проблема на стороне шлюза: без апстрима, без наказания номера, без ротации.
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "чтение тела: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("тело запроса превышает лимит %d МБ: сожмите содержимое или увеличьте server.max_body_mb и повторите", limit>>20))
		return
	}
	// Отладочный ключ: при установленном WB2A_DUMP_REQ сырое тело запроса со стороны апстрима пишется на диск для офлайн-бисекции строки срабатывания отпечатка.
	// Включать только при разборе блокировок отпечатков апстрима; без ключа — нулевые накладные расходы и никакой записи.
	// Пишем только «большие запросы» (больше половины лимита): мелкие пробы (вроде {"input":"hi"}) затёрли бы нужный диалог.
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(body)*2 >= int(limit) {
		if err := os.WriteFile("/app/data/last_request.json", body, 0o600); err != nil {
			log.Printf("ERR: [server] дамп запроса: %v", err)
		}
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// Статистика уровня запроса: на выходе всегда печатаем одну строку табличного лога (любой путь сюда дойдёт).
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// Липкость сессии: из тела извлекаем ключ сессии и ищем привязанный номер (не нашли/невалиден — stickyUID пуст, идём обычной ротацией).
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// In-flight аренда: выбранный номер занимает слот; освобождение централизовано на выходе функции (включая успешный return и panic).
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky отвязывает текущий липкий номер сессии (когда stickyUID непуст). Общий для «липкий номер недоступен/отобран» и fail.
	// Идемпотентно: при пустом stickyUID — пустая операция; чужую привязку не трогаем. stickyUID непуст только при Session != nil.
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail — общий путь веток неудачной ротации: освободить аренду + если упавший номер и есть липкий, отвязать (следующий запрос перераспределит).
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// Переписывание системного промпта (до отправки, до ротации; один раз на запрос).
	//   - custom: заменяем клиентские system/developer собственным промптом (ложные срабатывания отпечатков из system душим в источнике).
	//   - passthrough + период деградации: сразу идём с нейтральным промптом Degraded, без первого 400.
	//   - passthrough вне деградации: клиентский system передаём как есть (без переписывания).
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "passthrough" && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// Выбор номера: сначала липкий (PickByUID уже проверил health и незаполненность in-flight), иначе обычная ротация.
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUID(stickyUID)
			if acct == nil {
				// Липкий номер сейчас недоступен (охлаждение/занят) → отвязываем, в этот раз откатываемся к обычной ротации.
				unbindSticky()
			}
		}
		if acct == nil {
			// Модельно-осознанный выбор: если запрос несёт model, включаем освобождение от модельного охлаждения 6004
			// (внутри PickExcludingForModel при пустом model вырождается в PickExcluding).
			acct = h.cfg.Pool.PickExcludingForModel(tried, peek.Model)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// Занятие in-flight слота: Pick уже пропустил переполненные номера, здесь CAS страхует гонку за слот между горутинами.
		if !h.cfg.Pool.Acquire(acct.UID) {
			// Если отобрали именно липкий номер, сразу отвязываем и откатываемся к обычной ротации, чтобы в следующем круге не упереться в тот же
			// переполненный липкий номер и не тратить ещё один поход PickByUID (семантика та же, что отвязка в fail()/PickByUID-nil).
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // последний слот отобрала конкурирующая горутина → меняем номер
		}
		heldUID = acct.UID

		// token близок к истечению → сначала refresh (при неудаче охлаждаем и меняем номер)
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// Обновили успешно, но сохранить на диск не вышло: после рестарта будет старый token — такое надо показать
				log.Printf("ERR: [server] обновление chat uid=%s: не удалось сохранить auth: %v", logfmt.UID8(acct.UID), err)
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			// Джиттер сети: только меняем номер, счётчик пробоя не кормим (наказывать пробоем за ошибки транспорта при череде неудач слишком жестоко).
			// Клиент апстрима уже записал transport error в лог.
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			// Ложное срабатывание контент-фильтра (первая встреча в режиме passthrough): считаем отпечатком из system,
			// запускаем деградацию до ближайших 00:00 CST и повторяем в том же запросе с нейтральным промптом Degraded.
			// Если и второй раз заблокировано (модерацию триггерит сам контент пользователя) → отдаём клиенту по обычному пути ошибки.
			// Проблема контента, а не номера: applyErrorPolicy номер не наказывает (см. ветку ErrContentBlocked).
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // даже пул из одного номера получает шанс повтора (повтор деградации занимает одну попытку)
				releaseHeld()
				log.Printf("WARN: [server] блокировка контента (вероятно, ложное срабатывание отпечатка) -> повтор с нейтральным промптом")
				continue
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), peek.Model)
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// Липкость следует за финально успешным номером: номер успеха этого круга становится привязкой сессии (поверх старой).
		// Если липкий номер упал, а ротация на другой номер успешна — перепривязываем сессию на новый номер, следующий ход многодиалога уже не случаен.
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// Стрим: тело апстрима закрываем сразу по окончании сквозной передачи, чтобы defer не копил fd при ротации.
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = upstream.Stream(w, stats)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// Не смогли разобрать поток апстрима: клиент ещё ничего не видел — отвечаем 502 с причиной.
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		return
	}
	msg := "все аккаунты недоступны (охлаждение/отключены)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy применяет к номеру охлаждение/отключение/пробой по классу ошибки (финальная версия автомата).
// kind — единственный авторитетный класс (из upstream.Classify), здесь по исходному status второй раз не судим.
// Вызывается только из цикла ротации chatCompletions: вызывающий уже подготовил lastErr и собирается continue на другой номер.
//
// Семь путей, у каждого своя роль:
//   - ErrHardCredit → CooldownUntilTomorrow4AM: немедленное жёсткое охлаждение до 04:00 следующего дня (ждём восстановления подписанием).
//   - ErrSoftRate → по умолчанию Cooldown(CoolSoft, soft_rate) с экспоненциальным бэкоффом при повторах (потолок soft_rate_max);
//     если body апстрима — модельный 6004 со временем сброса → CooldownSoftForModel (until=настенные часы сброса,
//     потолок soft_rate_max, триггерную модель записываем для освобождения при смене модели).
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown фиксированные 60s): короткое охлаждение от лавины, без бэкоффа soft_rate.
//   - ErrSessionDead → Disable: сессия мертва, отключаем навсегда (нужен ручной перевход).
//   - ErrContentBlocked → номер не наказываем (без охлаждения/пробоя/NoteError), в режиме passthrough идём повтором деградации.
//   - ErrBadParams → номер не наказываем (без охлаждения/пробоя/NoteError, как ErrContentBlocked), но ротацию продолжаем.
//   - ErrServer → NoteError: кормим единый счётчик череды неудач fails + суммарный errTotal,
//     по достижении breakerThreshold срабатывает пробой (экспоненциальный бэкофф).
//   - Прочее (default: ErrClient/ErrNone) → только меняем номер без наказания (от лавины), пробой не кормим.
//
// body нужен только в ветке ErrSoftRate — распознать модельный лимит 6004 апстрима и разобрать время сброса; model — имя модели
// из запроса (при срабатывании 6004 записываем для будущего освобождения при смене модели).
//
// Выходы восстановления: CoolSoft/CoolHard самовосстанавливаются по истечении; пробой — по своему экспоненциальному сроку;
// успех (NoteSuccess) чистит fails/пробой; разморозка подписанием (ReenableIfCredits→reviveCoolingLocked) чистит только охлаждение, пробой не трогает.
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + ключевое слово апстрима «余额» (цитата-данные, не переводим) — кредиты исчерпаны: синхронно охлаждаем до 04:00 следующего дня (подписание в 09/21 восстановит),
		// асинхронная перепроверка не нужна (избыточна). Сразу меняем номер.
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足") // данные: причина-цитата апстрима, не переводим
	case upstream.ErrSoftRate:
		// Модельный 6004 со временем-цитатой апстрима «将在 … 重置» (данные, не переводим; issue #31): охлаждаем до прямо названных апстримом настенных часов сброса
		// (потолок soft_rate_max), триггерную модель записываем → для запросов **других моделей** этот номер от охлаждения освобождается.
		// Не разобрали (нет текста со временем / не 6004) → откатываемся к прежней базе 600s + текущий экспоненциальный рост.
		if upstream.IsModelRateLimit(body) {
			if resetAt, ok := upstream.ParseSoftRateReset(body); ok {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "6004 model rate limit")
				return
			}
		}
		// Остальные soft_rate: база мягкого охлаждения из soft_rate (по умолчанию 600s); при череде срабатываний на том же номере
		// внутри pool по softStreak идёт экспоненциальный бэкофф с потолком soft_rate_max.
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 — короткое охлаждение (мягкое), от лавины. Фиксированный notFoundCooldown, без бэкоффа soft_rate:
		// спорадическое отсутствие пути — не сигнал лимита, наказывать как лимит с эскалацией нельзя.
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		// Сбой апстрима 5xx: Classify уже отнёс ≥500 к ErrServer, здесь кормим счётчик пробоя (ручной status>=500 больше не пишем).
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// Блокировка контент-политикой (ложная): проблема контента, а не номера — не наказываем (без охлаждения/пробоя/NoteError).
		// В режиме passthrough обрабатывается повтором деградации внутри chatCompletions; в custom сюда вообще не попадаем.
	case upstream.ErrBadParams:
		// Тело не разобралось (400 + Unmarshal chat params failed / 11101): уходящий апстриму body
		// битый (обрезку шлюза уже убил 413, осталось кривое клиентское JSON). С другим номером всё равно будет 400,
		// номер не наказываем (без охлаждения/пробоя/NoteError, как ErrContentBlocked); но ротацию **продолжаем**
		// — у разных номеров могут быть разные права на модели, смена номера стоит ещё одной попытки.
	default:
		// Прочее (ErrClient/ErrNone): только меняем номер без наказания (от лавины), пробой не кормим.
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
