package upstream

import (
	"encoding/json"
	"testing"
)

// assistantRC извлекает reasoning_content каждого сообщения assistant из выходных messages (при отсутствии поля возвращает ""+false).
// Возвращаемый срез один-в-один соответствует сообщениям assistant в messages (не-assistant сообщения пропускаем).
func assistantRC(t *testing.T, out []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	var got []string
	msgs, _ := m["messages"].([]any)
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if v, ok := msg["reasoning_content"].(string); ok {
			got = append(got, v)
		} else {
			got = append(got, "<absent>")
		}
	}
	return got
}

// TestBackfillReasoningContentDeepSeek — мультираундовая консистентность DeepSeek: если исторические сообщения assistant несут
// следы reasoning, все сообщения assistant обязаны нести reasoning_content (string).
// Выравниваем под официальное поведение requiresReasoningContentOnAssistantMessages.
func TestBackfillReasoningContentDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
		// Проверяем только «число сообщений assistant» и «наличие поля reasoning_content в каждом»,
		// значение — любая строка (копия или пустая, конкретный кейс проверяет сам).
		wantCount int
		wantVals  []string // один-в-один к сообщениям assistant; пустая строка означает любую строку
	}{
		{"assistant с reasoning без reasoning_content → копируем",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a","reasoning":"thought text"}]}`,
			1, []string{"thought text"}},
		{"assistant с reasoning_content сохраняем как есть",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":"already there"}]}`,
			1, []string{"already there"}},
		{"смешанная сессия: всем недостающим assistant добиваем пустую строку",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a1","reasoning":"t1"},
				{"role":"user","content":"u2"},
				{"role":"assistant","content":"a2"}]}`,
			2, []string{"t1", ""}},
		{"пустой reasoning у assistant считаем отсутствием следов reasoning",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":""}]}`,
			1, []string{"<absent>"}},
		{"несколько assistant с reasoning — копируем все",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a1","reasoning":"r1"},
				{"role":"assistant","content":"a2","reasoning":"r2"}]}`,
			2, []string{"r1", "r2"}},
		{"нестроковый reasoning (число) обрабатываем как отсутствующий",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":123}]}`,
			1, []string{"<absent>"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantRC(t, out)
			if len(got) != c.wantCount {
				t.Fatalf("число сообщений assistant = %d, хотим %d (out=%s)", len(got), c.wantCount, out)
			}
			for i, want := range c.wantVals {
				if want == "" {
					continue // любая строка (и добитая пустая, и копия годятся, но поле обязано существовать)
				}
				if got[i] != want {
					t.Errorf("assistant[%d].reasoning_content = %q want %q (out=%s)", i, got[i], want, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNoTrace — в сессии нет никаких следов reasoning → без изменений:
// зря поле reasoning_content сообщениям assistant не добавляем.
func TestBackfillReasoningContentNoTrace(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"чистый текстовый assistant не трогаем",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"plain answer"}]}`},
		{"без сообщений assistant не трогаем",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"}]}`},
		{"без messages не трогаем",
			`{"model":"deepseek-v4-flash"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			for _, rc := range assistantRC(t, out) {
				if rc != "<absent>" {
					t.Errorf("без следов reasoning вдруг добавлен reasoning_content=%q (out=%s)", rc, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNonDeepSeek — не-deepseek модель без изменений:
// поле reasoning сохраняем как есть, reasoning_content не добавляем.
func TestBackfillReasoningContentNonDeepSeek(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	for _, rc := range assistantRC(t, out) {
		if rc != "<absent>" {
				t.Errorf("не-deepseek не должен делать backfill, получили reasoning_content=%q (out=%s)", rc, out)
		}
	}
}

// TestBackfillReasoningContentBothFields — одновременно reasoning и reasoning_content:
// главным считаем reasoning_content (не перекрываем), поле reasoning сохраняем (для совместимости) — выравниваем под правило matches клиента.
func TestBackfillReasoningContentBothFields(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","content":"a","reasoning":"t","reasoning_content":"existing"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "existing" {
		t.Errorf("reasoning_content должен брать уже имеющееся значение: получили %v (out=%s)", got, out)
	}
	// Заодно подтверждаем, что поле reasoning сохранилось как было.
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if r, ok := first["reasoning"].(string); !ok || r != "t" {
		t.Errorf("поле reasoning изменили: %v (out=%s)", first, out)
	}
}

// TestBackfillComposesWithInjectThinking — backfill и injectThinking независимы:
// при явном disabled reasoning_effort удаляется, но backfill срабатывает как обычно (мультираундовая консистентность не теряется из-за выключенной цепочки рассуждений).
func TestBackfillComposesWithInjectThinking(t *testing.T) {
	body := `{"model":"DEEPSEEK-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "thought" {
		t.Errorf("при disabled backfill всё равно должен сработать: получили %v (out=%s)", got, out)
	}
	if typ, present := getThinkingType(t, out); !present || typ != "disabled" {
		t.Errorf("thinking.type должен сохранить disabled, получили %q present=%v", typ, present)
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := objFieldString(t, out, k); ok {
			t.Errorf("%s должен быть удалён (при disabled)", k)
		}
	}
}
