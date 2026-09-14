// Package redisstore инкапсулирует персистентность Upstash (Redis) и даёт деградацию в память (Noop).
//
// Ограничение дизайна: Upstash ходит по публичному TLS, один RTT может быть 50~300ms, поэтому все записи —
// fire-and-forget (фоновая горутина + неудача только в debug-лог), чтения происходят только при старте
// (загрузка зеркала липких сессий, восстановление снапшота охлаждений/пробоя). Память — главная, Redis — вспомогательный.
//
// Без настроенного url / при неудаче соединения деградируем в Noop: вся функциональность работает как обычно (чисто режим памяти),
// наверх уходит лишь одна стартовая предупреждающая строка лога.
package redisstore

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyTTL — TTL по умолчанию для зеркала липких сессий + снапшота состояния (страховка на стороне redis от долгого залеживания грязных данных).
const keyTTL = 7 * 24 * time.Hour

// Store хранит только нужные на этом этапе методы. Контекст реализация строит внутри (чтения с коротким таймаутом, записи fire-and-forget).
type Store interface {
	// SetBind асинхронно зеркалирует привязку липкой сессии (key→uid), с TTL.
	SetBind(key, uid string, ttl time.Duration)
	// DelBind асинхронно удаляет привязку липкой сессии.
	DelBind(key string)
	// LoadBinds полностью читает привязки липких сессий (key→uid, префикс у key уже снят); вызывается только при старте (синхронно).
	// Для восстановления липкого отображения при холодном старте (чтобы рестарт не ронял липкость).
	LoadBinds() map[string]string
	// SaveState асинхронно пишет JSON-снапшот состояния пула (лежит рядом с локальным state.json, только как бэкап для восстановления).
	SaveState(data []byte)
	// LoadState читает снапшот состояния пула; вызывается только при старте (синхронно).
	LoadState() ([]byte, bool)
}

const (
	bindPrefix  = "wb2api:bind:"
	stateKey    = "wb2api:state"
	readTimeout = 3 * time.Second
)

// New строит Store по url+token.
//   - пустой url → Noop (режим чистой памяти)
//   - url уже полный rediss:// URL — парсим напрямую через ParseURL; иначе собираем из token rediss://default:token@host:6379
//   - Ping не удался → Noop + стартовое предупреждение (жёсткое требование деградации: недоступность Redis не должна ронять процесс)
func New(url, token string) Store {
	if url == "" {
		log.Printf("[redisstore] upstash не настроен, входим в режим чистой памяти (деградация в Noop)")
		return Noop{}
	}

	full := normalizeURL(url, token)
	opt, err := redis.ParseURL(full)
	if err != nil {
		log.Printf("[redisstore] предупреждение: не разобрали строку соединения redis (%v), деградируем в Noop", err)
		return Noop{}
	}
	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = readTimeout
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("[redisstore] предупреждение: соединение с upstash не удалось (%v), деградируем в Noop (режим чистой памяти)", err)
		_ = client.Close()
		return Noop{}
	}
	log.Printf("[redisstore] upstash подключён (addr=%s)", opt.Addr)
	return &Upstash{client: client}
}

// normalizeURL нормализует url+token в полный rediss:// URL, готовый к прямому ParseURL.
// Если url уже содержит scheme (rediss://, redis://, https://...upstash.io и т.п.):
//   - rediss:// или redis:// возвращаем как есть (уже полная строка соединения)
//   - остальное (например https://xxx.upstash.io): отрезаем префикс до "://" и берём только host, затем собираем
//     собираем "rediss://default:<token>@<host>:6379"
func normalizeURL(url, token string) string {
	if len(url) >= 8 && (url[:8] == "rediss:/" || url[:7] == "redis:/") {
		return url
	}
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return "rediss://default:" + token + "@" + host + ":6379"
}

// Upstash — настоящая реализация: обёртка над redis.Client.
type Upstash struct {
	client *redis.Client
}

func bindKey(key string) string { return bindPrefix + key }

// SetBind асинхронно зеркалирует привязку липкой сессии.
func (u *Upstash) SetBind(key, uid string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, bindKey(key), uid, ttl).Err(); err != nil {
			log.Printf("[redisstore] отладка: SetBind %s: %v", key, err)
		}
	}()
}

// DelBind асинхронно удаляет привязку липкой сессии.
func (u *Upstash) DelBind(key string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Del(ctx, bindKey(key)).Err(); err != nil {
			log.Printf("[redisstore] отладка: DelBind %s: %v", key, err)
		}
	}()
}

// SaveState асинхронно пишет JSON-снапшот состояния пула.
func (u *Upstash) SaveState(data []byte) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, stateKey, data, keyTTL).Err(); err != nil {
			log.Printf("[redisstore] отладка: SaveState: %v", err)
		}
	}()
}

// LoadState синхронно читает снапшот состояния пула.
func (u *Upstash) LoadState() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	v, err := u.client.Get(ctx, stateKey).Bytes()
	if err != nil {
		return nil, false
	}
	return v, true
}

// LoadBinds полностью читает привязки липких сессий (префикс SCAN bind:*).
func (u *Upstash) LoadBinds() map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	iter := u.client.Scan(ctx, 0, bindPrefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		v, err := u.client.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		out[strings.TrimPrefix(key, bindPrefix)] = v
	}
	return out
}

// Noop — деградация в чистую память: все методы с пустой реализацией.
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration) {}
func (Noop) DelBind(string)                        {}
func (Noop) LoadBinds() map[string]string          { return nil }
func (Noop) SaveState([]byte)                      {}
func (Noop) LoadState() ([]byte, bool)             { return nil, false }
