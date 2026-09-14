package pool

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestNoteSessionDeadThresholdNotReached: первые 2 последовательных 12153 не отключают (защита от ошибочных).
func TestNoteSessionDeadThresholdNotReached(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.NoteSessionDead("u1") {
		t.Fatal("1-й подряд 12153 не должен отключать")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("2-й подряд 12153 не должен отключать")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("2 подряд 12153 не должны отключать: %+v", st)
	}
	// Пока порог не достигнут, аккаунт всё ещё выбираем (ошибка keepalive не загрязняет выбор).
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("аккаунт должен оставаться выбираемым, got %+v", got)
	}
}

// TestNoteSessionDeadDisablesAtThird: 3-й подряд 12153 → отключение и сброс счёта.
func TestNoteSessionDeadDisablesAtThird(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if !p.NoteSessionDead("u1") {
		t.Fatal("3-й подряд 12153 должен отключать")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("после 3-го должен быть disabled: %+v", st)
	}
	if st.Reason != "12153 session dead" {
		t.Errorf("reason=%q want 12153 session dead", st.Reason)
	}
	// После отключения больше не выбирается.
	if p.Pick() != nil {
		t.Fatal("отключённый аккаунт нельзя выбрать")
	}
}

// TestClearSessionDeadResetsCount: промежуточный успех (успех refresh) сбрасывает счёт, дальше считаем заново с 1.
func TestClearSessionDeadResetsCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.ClearSessionDead("u1") // имитируем успех refresh: сброс счёта
	if p.NoteSessionDead("u1") {
		t.Fatal("после сброса счёта 1-й раз не должен отключать")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("после сброса счёта 2-й раз не должен отключать")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("после сброса счёта 3-й должен отключать (с 1 заново набралось 3)")
	}
}

// TestNoteSuccessClearsSessionDeadCount: любой успех (успех chat) — тоже сильное доказательство живости сессии,
// так же сбрасывает счёт — вровень с успешным refresh.
func TestNoteSuccessClearsSessionDeadCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSuccess("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("после сброса счёта успехом 1-й раз не должен отключать")
	}
}

// TestReviveDisabled — вход возрождения: снимает disabled + reason + счёт ошибочных, аккаунт возвращается в пул.
func TestReviveDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1") // отключающий вызов
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: должен уже быть отключён")
	}
	p.ReviveDisabled("u1")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("revive должен снять disabled: %+v", st)
	}
	if st.Reason != "" {
		t.Errorf("reason=%q want пусто (revive очищает reason)", st.Reason)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("после возрождения аккаунт должен вернуться в пул, got %+v", got)
	}
	// Счёт ошибочных заодно обнуляется: после возрождения отключаем только после заново набранных 3.
	p.NoteSessionDead("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("после возрождения 2-й не должен отключать (счёт заново)")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("после возрождения 3-й должен отключать (заново набралось 3)")
	}
}

// TestReviveDisabledPersists: снятие disabled + reason возрождением сохраняется на диск (после рестарта не откатывается).
func TestReviveDisabledPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Disabled || st.Reason != "" {
		t.Fatalf("revive должен сохраняться (disabled=%v reason=%q) ok=%v", st.Disabled, st.Reason, ok)
	}
}

// TestStatusDisabledReasonDisabled: когда disabled, Status раскрывает disabled_reason.
func TestStatusDisabledReasonDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead (disabled=%v)", st.DisabledReason, st.Disabled)
	}
}

// TestStatusDisabledReasonClearedByRevive: после возрождения disabled_reason пустеет.
func TestStatusDisabledReasonClearedByRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.DisabledReason != "" {
		t.Errorf("после revive disabled_reason=%q want пусто", st.DisabledReason)
	}
	if st.Reason != "" {
		t.Errorf("после revive reason=%q want пусто", st.Reason)
	}
}
