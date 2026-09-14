// Package config хранит общие для команд секции конфигурации и логику значений по умолчанию.
//
// Причина — issue #49: разовый триггер cmd/activity когда-то скопировал себе урезанную структуру schedule,
// делал только json.Unmarshal без значений по умолчанию, отсутствующий ключ → нулевое значение Go 0 → нормализация scheduler в 1,
// а в основной программе cmd/server по умолчанию 5 записей — дрейф. Секция Schedule + значения по умолчанию/нормализация вынесены в этот пакет,
// обе команды используют одно определение, источник дрейфа устранён.
package config

import "fmt"

// Schedule — секция расписания (объект "schedule" в config.json).
//
// Четыре независимых расписания: чекин / отчёт об активности / путешествие кота / keepalive токена.
// cmd/server и cmd/activity используют одну структуру, значения по умолчанию задаёт DefaultSchedule,
// нормализацию пропусков делает Normalize — обе команды идут по одной семантике, копий больше нет.
type Schedule struct {
	CheckinHours   []int `json:"checkin_hours"`   // [9,21]
	TravelHours    []int `json:"travel_hours"`    // [9,21]
	ActivityHours  []int `json:"activity_hours"`  // [10]
	KeepaliveHours []int `json:"keepalive_hours"` // [22]
	// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled — явные выключатели (по умолчанию true).
	//
	// Почему отдельный bool, а не пустой массив/сентинел со смыслом "выключено":
	//   - пустой массив и null в старой семантике уже заняты под "не настроено → откат к умолчанию", смена трактовки молча перевернёт
	//     поведение всех старых config (пользователь удалил одну строку — и случайно выключил чекин); bool по умолчанию true
	//     никак не влияет на старые конфигурации, полная обратная совместимость.
	//   - выключатель отделён от значений: при выключении явно заданные часы сохраняются, повторное включение не требует дописывать конфиг.
	//   - не нужно угадывать сентинелы (вроде [-1]), недопустимые часы всегда ошибка с подсказкой использовать этот выключатель.
	CheckinEnabled   bool `json:"checkin_enabled"`   // по умолчанию true; false = выключить чекин
	TravelEnabled    bool `json:"travel_enabled"`    // по умолчанию true; false = полностью остановить путешествие кота
	ActivityEnabled  bool `json:"activity_enabled"`  // по умолчанию true; false = остановить отчёты об активности
	KeepaliveEnabled bool `json:"keepalive_enabled"` // по умолчанию true; false = выключить keepalive токена
	// ActivityReportCount — число отчётов об активности на номер за раз: чтобы забрать кота, нужно 5 диалогов,
	// по умолчанию 5 записей закрывают chat_5; 0/пропуск=1 для совместимости со старым поведением.
	ActivityReportCount int `json:"activity_report_count"`
	// Путешествие кота сняло с учёта travel_interval_minutes: теперь у путешествия отдельное расписание (travel_hours).
	// Старый ключ в config из-за неизвестных JSON-полей просто игнорируется, без ошибки.
}

// DefaultSchedule возвращает значения секции расписания по умолчанию.
//
// Выключатели «по умолчанию true» реализованы здесь: вызывающий код сначала берёт DefaultSchedule, потом накрывает json.Unmarshal,
// отсутствующий ключ (или null) оставляет поле как было — true, только явный false выключает.
// ActivityReportCount по умолчанию 5: чтобы забрать кота, нужно 5 диалогов, 5 записей подряд закрывают chat_5.
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:        []int{9, 21},
		TravelHours:         []int{9, 21},
		ActivityHours:       []int{10},
		KeepaliveHours:       []int{22},
		CheckinEnabled:      true,
		TravelEnabled:       true,
		ActivityEnabled:     true,
		KeepaliveEnabled:    true,
		ActivityReportCount: 5, // чтобы забрать кота, нужно 5 диалогов, 5 записей подряд закрывают chat_5
	}
}

// Normalize нормализует секцию расписания: пустые массивы/null откатываются к часам по умолчанию, ActivityReportCount нормализуется, диапазон часов проверяется.
//
// Пустой массив и null при десериализации затирают значения расписания из DefaultSchedule (отсутствие ключа — сохраняет), здесь добиваем.
// Пусто = не настроено → откат к умолчанию; «выключить» — только через *_enabled=false, одно с другим не смешивается.
//
// ActivityReportCount: 0/отрицательное → 1 запись (совместимость со старым поведением: один отчёт на номер в день зажигал серию входов).
// Обратите внимание: это семантика совместимости «явный 0 = старое поведение», совпадающая с нормализацией scheduler.New <=0 → 1;
// «по умолчанию = 5» подставляет DefaultSchedule до Unmarshal — другой путь, их не объединяем.
func (s *Schedule) Normalize() error {
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = []int{9, 21}
	}
	if len(s.TravelHours) == 0 {
		s.TravelHours = []int{9, 21}
	}
	if len(s.ActivityHours) == 0 {
		s.ActivityHours = []int{10}
	}
	if len(s.KeepaliveHours) == 0 {
		s.KeepaliveHours = []int{22}
	}
	// 0/отрицательное → 1 запись (совместимость со старым поведением: один отчёт на номер в день зажигал серию входов).
	if s.ActivityReportCount <= 0 {
		s.ActivityReportCount = 1
	}
	return s.validateHours()
}

// validateHours проверяет, что часы расписания в диапазоне 0-23.
//
// Почему не сентинелы вроде `[-1]` со смыслом "выключено": недопустимый час при молчаливом проглатывании заставит пользователя думать, что чекин выключен,
// а на деле он может выполняться как обычно в другой час; здесь быстрый отказ с указанием правильного выключателя в тексте ошибки
// (checkin_enabled / keepalive_enabled), чтобы не гадать с сентинелами.
func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", s.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", s.ActivityHours); err != nil {
		return err
	}
	return checkHourRange("schedule.keepalive_hours", "keepalive_enabled", s.KeepaliveHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d — недопустимый час (0-23); чтобы выключить задачу, задайте schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
