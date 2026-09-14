package pool

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// Этот файл — количественные бенчмарки проверки производительности группы P (воспроизводимо через go test -bench), выводы см. в REVIEW-conflicts-perf.md.

// benchPool строит пул на 46 аккаунтов (вровень с продмасштабом), все healthy и с разными credits.
func benchPool(b *testing.B) *Pool {
	p := New("")
	for i := 0; i < 46; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%02d", i)})
		p.SetCredits(fmt.Sprintf("u%02d", i), int64(1000-i*13%900))
	}
	return p
}

// BenchmarkPick46Accounts P1: время одного прохода при полном скане 46 аккаунтов + полной сортировке + трёхфакторной взвешенной жеребьёвке.
func BenchmarkPick46Accounts(b *testing.B) {
	p := benchPool(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Pick()
	}
}

// BenchmarkStateSerialize46Accounts P2: сериализация state.json 46 аккаунтов (stateOverviewLocked + MarshalIndent).
func BenchmarkStateSerialize46Accounts(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.MarshalIndent(sf, "", "  "); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStateSerializeCompact46 контроль к P2: время json.Marshal (без отступов), нижняя граница write amplification.
func BenchmarkStateSerializeCompact46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(sf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnapshotMarshal46 по теме P2/P4: цена сериализации лишнего зеркала snapshot (с savedAt) при записи saveLocked на диск.
func BenchmarkSnapshotMarshal46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now := time.Now()
		if _, err := json.Marshal(snapshot{stateFile: sf, SavedAt: now}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGoroutineSpawn количественно по P4: нижняя граница CPU-цены одного старта fire-and-forget goroutine
// (соответствует цене порождения goroutine на каждое зеркало Session.SetBind / pool SaveState, без сети,
//
//	сеть идёт fire-and-forget с таймаутом 5s).
func BenchmarkGoroutineSpawn(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		go func() { _ = 1 }()
	}
}
