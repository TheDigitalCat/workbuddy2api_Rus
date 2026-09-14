package config

import (
	"strings"
	"testing"
)

// TestDefaultScheduleActivityCount: по умолчанию ActivityReportCount=5 (вровень с cmd/server Default()).
// issue #49: cmd/activity когда-то копировал структуру без значений по умолчанию, пропуск откатывался к 1 и дрейфовал от 5 в server.
func TestDefaultScheduleActivityCount(t *testing.T) {
	s := DefaultSchedule()
	if s.ActivityReportCount != 5 {
		t.Errorf("default ActivityReportCount=%d want 5", s.ActivityReportCount)
	}
	if !s.CheckinEnabled || !s.TravelEnabled || !s.ActivityEnabled || !s.KeepaliveEnabled {
		t.Errorf("all switches must default true: %+v", s)
	}
	if len(s.CheckinHours) != 2 || s.CheckinHours[0] != 9 || s.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want [9 21]", s.CheckinHours)
	}
	if len(s.ActivityHours) != 1 || s.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", s.ActivityHours)
	}
}

// TestNormalizeScheduleThreeStates — три состояния пропуск/явный 0/явное N:
//   - пропуск (пустой schedule) → ActivityReportCount сохраняет 5 из DefaultSchedule, hours откатываются к умолчанию.
//   - явный 0 → нормализуется в 1 (совместимость со старым поведением).
//   - явное N → сохраняется N.
func TestNormalizeScheduleThreeStates(t *testing.T) {
	cases := []struct {
		name string
		s    Schedule
		want int
	}{
		{"absent_keeps_default", DefaultSchedule(), 5},
		{"explicit_zero_to_one", Schedule{ActivityReportCount: 0}, 1},
		{"explicit_negative_to_one", Schedule{ActivityReportCount: -3}, 1},
		{"explicit_n", Schedule{ActivityReportCount: 8}, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			if err := s.Normalize(); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if s.ActivityReportCount != tc.want {
				t.Errorf("ActivityReportCount=%d want %d", s.ActivityReportCount, tc.want)
			}
		})
	}
}

// TestNormalizeScheduleEmptyHoursFallback: пустые массивы/пропущенные hours всегда откатываются к умолчанию.
func TestNormalizeScheduleEmptyHoursFallback(t *testing.T) {
	s := Schedule{ActivityReportCount: 5} // hours — нулевые значения (не настроены)
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(s.CheckinHours) != 2 || s.CheckinHours[0] != 9 || s.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want [9 21]", s.CheckinHours)
	}
	if len(s.KeepaliveHours) != 1 || s.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want [22]", s.KeepaliveHours)
	}
	if len(s.TravelHours) != 2 || s.TravelHours[0] != 9 || s.TravelHours[1] != 21 {
		t.Errorf("travel_hours=%v want [9 21]", s.TravelHours)
	}
	if len(s.ActivityHours) != 1 || s.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", s.ActivityHours)
	}
}

// TestNormalizeScheduleInvalidHour: недопустимый час — быстрый отказ с указанием правильного выключателя.
func TestNormalizeScheduleInvalidHour(t *testing.T) {
	cases := []struct {
		s           Schedule
		wantSwitch  string
	}{
		{Schedule{CheckinHours: []int{25}}, "checkin_enabled"},
		{Schedule{CheckinHours: []int{-1}}, "checkin_enabled"},
		{Schedule{KeepaliveHours: []int{24}}, "keepalive_enabled"},
		{Schedule{TravelHours: []int{-1}}, "travel_enabled"},
		{Schedule{ActivityHours: []int{24}}, "activity_enabled"},
	}
	for _, tc := range cases {
		err := tc.s.Normalize()
		if err == nil {
			t.Errorf("want error for %+v", tc.s)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error %q should point at schedule.%s", err.Error(), tc.wantSwitch)
		}
	}
}
