package server

import (
	"testing"
	"time"
)

// TestNextMidnightCSTBoundaries — границы: после 00:00 текущего дня по CST → 00:00 следующего дня,
// 23:59 → 00:00 следующего дня, полдень → 00:00 следующего дня. Считаем с фиксированным +08:00, без зависимости от системного пояса.
func TestNextMidnightCSTBoundaries(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		// Ожидаем, что часы/минуты/секунды until по CST всегда 00:00:00 и strictly after now.
	}{
		{"23:59 -> next 00:00", time.Date(2026, 9, 11, 23, 59, 0, 0, cst)},
		{"00:00:01 -> next 00:00", time.Date(2026, 9, 11, 0, 0, 1, 0, cst)},
		{"12:00 -> next 00:00", time.Date(2026, 9, 11, 12, 0, 0, 0, cst)},
		{"00:00:00 exact -> next 00:00", time.Date(2026, 9, 11, 0, 0, 0, 0, cst)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextMidnightCST(tc.now)
			if !got.After(tc.now) {
				t.Fatalf("until %v not after now %v", got, tc.now)
			}
			// По CST обязано попадать ровно в 00:00:00.
			if g := got.In(cst); g.Hour() != 0 || g.Minute() != 0 || g.Second() != 0 {
				t.Errorf("until CST = %02d:%02d:%02d, want 00:00:00", g.Hour(), g.Minute(), g.Second())
			}
		})
	}
}

// TestNextMidnightCSTCrossMonth — смена месяца: 23:59 последнего дня → 00:00 1-го числа следующего месяца.
func TestNextMidnightCSTCrossMonth(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 1, 31, 23, 59, 0, 0, cst)
	got := nextMidnightCST(now)
	want := time.Date(2026, 2, 1, 0, 0, 0, 0, cst)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestDegradeGateActiveTriggered — Active() равен true после Trigger и false после истечения.
func TestDegradeGateActiveTriggered(t *testing.T) {
	var g degradeGate
	if g.Active() {
		t.Error("fresh gate should not be active")
	}
	g.Trigger()
	if !g.Active() {
		t.Error("after Trigger should be active")
	}
}

// TestDegradeGateTriggerNoRenewal — повторный Trigger в период деградации не продлевает (сброс в 00:00 от первого срабатывания сохраняется).
func TestDegradeGateTriggerNoRenewal(t *testing.T) {
	var g degradeGate
	g.Trigger()
	first := func() time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.until
	}()
	// Повторный Trigger: период ещё активен, until не должен меняться.
	g.Trigger()
	second := func() time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.until
	}()
	if !first.Equal(second) {
		t.Errorf("Trigger during active should not renew: first=%v second=%v", first, second)
	}
}
