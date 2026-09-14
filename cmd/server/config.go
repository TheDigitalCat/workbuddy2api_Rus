// config.go Загрузка JSON-конфигурации + переопределение переменными окружения.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/prompt"
)

// Config Верхнеуровневая конфигурация.
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // пусто = без аутентификации
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Server struct {
		// MaxBodyMB Предел размера тела чат-запроса (в МБ, по умолчанию 8).
		// Тело сверх лимита сразу отвечает 413 request_body_too_large, а не усекается молча перед отправкой апстриму
		// (issue #41: усечённый JSON ронял разбор апстрима с unexpected EOF, а шлюз при этом штрафовал аккаунт).
		// 0/отрицательные значения недопустимы → normalize возвращает ошибку.
		MaxBodyMB int `json:"max_body_mb"`
	} `json:"server"`

	Cooldown struct {
		// Три исторических ключа hard_credit / err_threshold / err_cooldown выведены из употребления:
		// жёсткий кулдаун зафиксирован на 04:00 следующих суток (CooldownUntilTomorrow4AM), семантика
		// последовательных ошибок переехала в автоматический выключатель.
		// Старые ключи в config игнорируются как неизвестные JSON-поля без ошибки.
		SoftRate string `json:"soft_rate"` // "600s", база мягкого лимитного кулдауна
		// SoftRateMax Потолок экспоненциального роста мягкого кулдауна, по умолчанию "2h".
		// Пустое значение откатывается к умолчанию, недопустимое — ошибка (как у soft_rate).
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	Upstream struct {
		// TimeoutSeconds Общий лимит времени коротких RPC (refresh/checkin/balance/FetchModels), по умолчанию 120.
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds Лимит ожидания первых байт (заголовков) SSE чата; <=0 наследует TimeoutSeconds.
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds Лимит простоя внутри SSE-потока чата (активная отдача данных продлевает); <=0 откатывается к 300 по умолчанию.
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent Переопределение исходящего User-Agent (пусто = текущий `CLI/2.63.2 CodeBuddy/2.63.2`).
		// Действует на все исходящие запросы: chat/refresh/checkin/balance/report/travel/FetchModels.
		// issue #42: колонка «способ использования» на сайте строится по серверной атрибуции исходящего UA,
		// десктопный UA официального WorkBuddy — `WorkBuddy/<version>`. Из соображений очистки отпечатков:
		// по умолчанию поведение не меняется (настраивается, а не зашито),
		// перезапись применяется только при явной настройке пользователем.
		UserAgent string `json:"user_agent"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints Десенситизация отпечатков из чёрного списка в исходящем теле (по умолчанию true; false — полное исходное поведение).
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode custom (по умолчанию) = шлюз заменяет client system/developer собственным системным промптом;
		// passthrough = исходный client system пробрасывается как есть (при деградированном повторе всё равно переключается на Degraded).
		Mode string `json:"mode"` // "custom" / "passthrough"
		// File Путь к файлу промпта; пусто = встроенный defaultprompt.md;
		// непустой путь, который нельзя прочитать → ошибка запуска (fail fast, чтобы не откатываться молча на встроенный).
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText Разобранный текст системного промпта (используется в режиме custom).
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // пусто = чисто in-memory режим; поддерживается полный rediss:// URL либо https://xxx.upstash.io host
		Token string `json:"token"` // когда url — не полная строка подключения, собирается rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // Максимум одновременных запросов на аккаунт, 0 = без лимита
		BreakerThreshold   int     `json:"breaker_threshold"`    // Число последовательных ошибок до срабатывания выключателя, по умолчанию 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // Базовая длительность размыкания, по умолчанию "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // Потолок экспоненциального роста, по умолчанию "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // Компенсация простоя: +0.5 веса за каждый неиспользованный час
		IdleWeightMax      float64 `json:"idle_weight_max"`      // Потолок компенсации простоя, по умолчанию 5.0
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // По умолчанию true
		TTL        string `json:"ttl"`         // TTL привязки сессии, по умолчанию "30m"
		GCInterval string `json:"gc_interval"` // Период GC сессий, по умолчанию "5m"
	} `json:"session_sticky"`

	// Разобранное (вычисленное)
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
}

// Default Конфигурация по умолчанию.
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.MaxBodyMB = 8 // Предел тела запроса по умолчанию 8 МБ
	// Значения по умолчанию для раздела расписания централизованно ведёт internal/config (общие для cmd/server и cmd/activity,
	// устраняет дрейф значений по умолчанию из issue #49).
	c.Schedule = config.DefaultSchedule()
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds по умолчанию 0 (состояние «не задано»), откат см. в normalize().
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "custom" // По умолчанию custom: собственный промпт шлюза устраняет ложные срабатывания system-отпечатков в источнике
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load Чтение из файла с переопределением переменными окружения WB2A_*.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("чтение конфигурации: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("разбор конфигурации: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.MaxBodyMB = n
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
}

func (c *Config) normalize() error {
	var err error
	// max_body_mb недопустим (0/отрицательное) — сразу ошибка: 0, молча принятый за 8 МБ по умолчанию, выглядит как «без лимита»,
	// а крупные запросы потом молча получают 413 — лучше fail fast с подсказкой явно задать больший лимит.
	if c.Server.MaxBodyMB <= 0 {
		return fmt.Errorf("server.max_body_mb: %d недопустимо (требуется положительное целое, единица МБ)", c.Server.MaxBodyMB)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// Пустое значение откатывается к 2h по умолчанию (Default() уже выставил; страховка покрывает явный "" и обход Default()).
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header по умолчанию наследует timeout (сохраняет прежнюю семантику «смены номера до первого байта»); idle по умолчанию берёт большое встроенное значение.
	// Договорённость по ТЗ: 0 всегда означает «не задано» и ведёт к умолчанию, настоящее «отключение» отложено на будущее (во избежание двусмысленности).
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// Нормализация раздела расписания (пустые массивы откатываются к умолчанию, нормализация ActivityReportCount, проверка диапазона часов)
	// единой реализацией в internal/config, общая семантика для cmd/server и cmd/activity.
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt Проверяет prompt.mode и загружает текст промпта из file (режим custom).
//
// Недопустимый mode (не custom/passthrough) — ошибка запуска, чтобы не откатываться молча к какой-то из веток;
// в режиме custom непустой, но нечитаемый file → ошибка (fail fast), пустой file → встроенное значение по умолчанию.
// Режим passthrough текст не загружает (исходный client system пробрасывается как есть, текст используется при деградации как prompt.Degraded).
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "custom":
		c.Prompt.Mode = "custom"
	case "passthrough":
		c.Prompt.Mode = "passthrough"
	default:
		return fmt.Errorf("prompt.mode: %q недопустимое значение (custom / passthrough)", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

