// activity Разовый триггер: один прогон RunActivityNow (5 отправок + связка с приютом кота), нужен для проверки контура отчёта активности после деплоя, не резидент.
//
// Использование (ручной запуск после деплоя):
//
//	# В контейнере: сначала скопировать внутрь, затем exec
//	docker cp activity-run workbuddy2api:/tmp/activity-run
//	docker exec -w /app workbuddy2api /tmp/activity-run
//
//	# Локально: в корне проекта (нужны config.json + auths/ + data/) прямой run
//	go run ./cmd/activity
//
// Читает config.json из рабочего каталога (auth_dir / state_file / schedule / upstream.timeout_seconds),
// загружает auths, затем строит pool + upstream и вызывает scheduler.RunActivityNow для немедленного однократного выполнения.
//
// Раздел schedule переиспользует ту же структуру Schedule из internal/config + значения по умолчанию (issue #49):
// общий с cmd/server, больше никакого копирования и дрейфа значений по умолчанию.
package main

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/config"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// cfgFile Берёт только нужные этому инструменту поля; раздел Schedule — напрямую internal/config.Schedule
// (общий источник с cmd/server), остальные разделы оставлены компактными инлайн.
type cfgFile struct {
	AuthDir   string            `json:"auth_dir"`
	StateFile string            `json:"state_file"`
	Schedule  config.Schedule   `json:"schedule"`
	Upstream  struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`
}

func main() {
	raw, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatalf("чтение конфигурации: %v", err)
	}
	// Сначала выставить значения по умолчанию, затем Unmarshal: при отсутствии ключей (или null) поля сохраняют значения по умолчанию,
	// тем же путём, что Load в cmd/server — по умолчанию activity_report_count=5, а не нулевое значение Go 0.
	c := cfgFile{Schedule: config.DefaultSchedule()}
	if err := json.Unmarshal(raw, &c); err != nil {
		log.Fatalf("разбор конфигурации: %v", err)
	}
	if err := c.Schedule.Normalize(); err != nil {
		log.Fatalf("нормализация расписания: %v", err)
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "data/state.json"
	}
	log.Printf("число отчётов активности=%d", c.Schedule.ActivityReportCount)

	auths, err := auth.LoadDir(c.AuthDir)
	if err != nil {
		log.Fatalf("загрузка аккаунтов: %v", err)
	}
	log.Printf("загружено %d аккаунт(а/ов)", len(auths))

	p := pool.New(c.StateFile)
	p.SyncToDir(auths)

	up := upstream.New()
	if c.Upstream.TimeoutSeconds > 0 {
		up.HTTP.Timeout = time.Duration(c.Upstream.TimeoutSeconds) * time.Second
	}

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        c.Schedule.CheckinHours,
		TravelHours:         c.Schedule.TravelHours,
		ActivityHours:       c.Schedule.ActivityHours,
		KeepaliveHours:      c.Schedule.KeepaliveHours,
		ActivityReportCount: c.Schedule.ActivityReportCount,
	})
	sch.RunActivityNow()
	log.Printf("прогон активности завершён")
}
