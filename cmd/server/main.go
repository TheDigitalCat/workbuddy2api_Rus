// main.go Точка входа workbuddy2api: загрузка конфигурации, построение пула, запуск планировщика и HTTP-сервиса.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "путь к JSON-файлу конфигурации")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// Если файла конфигурации нет — один шанс запуститься на чистых значениях по умолчанию + env
		if os.IsNotExist(err) {
			log.Printf("конфигурация %s не найдена, используются defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("загрузка конфигурации: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("загрузка аккаунтов: %v", err)
	}
	log.Printf("загружено %d аккаунт(а/ов) из %s", len(auths), cfg.AuthDir)

	// redisstore: не настроен/недоступен → Noop (чисто in-memory режим, весь функционал работает как обычно).
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // Перед выходом процесса принудительно сбросить на диск (фоновый flush каждые 5s, при выходе — ещё один)
	p.SetStore(store)
	p.RestoreFromSnapshot() // Восстановление по новизне: снапшот Redis применяется, только если новее локального, иначе приоритет у локального
	p.SyncToDir(auths)      // Сверка с каталогом auths: новые аккаунты добавляются, аккаунты удалённых файлов исключаются (состояние сохраняется)

	// Автовыключатель + лимит одновременных запросов + взвешенная настройка по трём факторам (внедряются из config, неположительные значения откатываются к умолчанию).
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // Потолок экспоненциального роста мягкого кулдауна (soft_rate_max, по умолчанию 2h)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// Маршрутизация с привязкой сессии (отключаемая).
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
		})
		sessRouter.LoadFromStore() // Восстановление привязок из Redis при старте (единственное место чтения)
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// Общий лимит времени коротких RPC (refresh/checkin/balance/FetchModels), семантика неизменна.
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// Лимит ожидания заголовков чат-SSE до первого байта: cfg уже нормализован (по умолчанию наследует timeout_seconds).
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// Лимит простоя внутри чат-SSE (контроль простоя чтения S3).
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// Переопределение исходящего UA (issue #42): перезаписывается только при непустом значении, пусто = текущий clientUA (с учётом очистки отпечатков).
	up.UserAgent = cfg.Upstream.UserAgent

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("вход (checkin) отключён (schedule.checkin_enabled=false)")
	default:
		log.Printf("вход (checkin) включён: часы %v (вход + разморозка запросом баланса)", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("путешествие кота отключено (schedule.travel_enabled=false)")
	default:
		log.Printf("путешествие кота включено: часы %v (отдельное расписание: приютить / отправить / забрать награду)", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("отчёт активности отключён (schedule.activity_enabled=false)")
	default:
		log.Printf("отчёт активности включён: часы %v (по %d сообщений на номер, подсветка серии входов + добор порога диалогов для приюта кота)", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("поддержание token отключено (schedule.keepalive_enabled=false)")
	} else {
		log.Printf("поддержание token включено: часы %v", cfg.Schedule.KeepaliveHours)
	}

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		MaxBodyBytes: int64(cfg.Server.MaxBodyMB) << 20, // МБ → байты
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // По сигналу: сначала сброс на диск, затем graceful shutdown
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api слушает %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("пока")
}
