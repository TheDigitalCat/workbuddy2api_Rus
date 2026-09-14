package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// getThinkingType извлекает thinking.type из выходного body (при отсутствии поля возвращает пустую строку + наличие).
func getThinkingType(t *testing.T, out []byte) (typ string, present bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	th, ok := m["thinking"].(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := th["type"].(string)
	if !ok {
		return "", true
	}
	return s, true
}

// TestInjectThinkingDeepSeekEnabled — инжект включения мышления: для deepseek-моделей без thinking в теле запроса
// обязаны инжектить {type:"enabled"}, иначе апстрим по умолчанию отвечает без мышления (цепочка не показывается).
func TestInjectThinkingDeepSeekEnabled(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantTyp string
	}{
		{"deepseek без thinking инжектим enabled",
			`{"model":"deepseek-v4-flash","messages":[]}`, "enabled"},
		{"DeepSeek регистронезависимо",
			`{"model":"DeepSeek-v4.1-flash","messages":[]}`, "enabled"},
		{"DEEPSEEK капсом регистронезависимо",
			`{"model":"DEEPSEEK-R1","messages":[]}`, "enabled"},
		{"deepseek с reasoning_effort без thinking инжектим enabled",
			`{"model":"deepseek-v4-flash","reasoning_effort":"medium","messages":[]}`, "enabled"},
		{"deepseek с пустым объектом thinking добиваем enabled",
			`{"model":"deepseek-v4-flash","thinking":{},"messages":[]}`, "enabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present {
				t.Fatalf("поле thinking отсутствует (out=%s)", out)
			}
			if typ != c.wantTyp {
				t.Errorf("thinking.type = %q want %q (out=%s)", typ, c.wantTyp, out)
			}
		})
	}
}

// TestInjectThinkingDefaultEffort — главное доказательство добивочного фикса: голый запрос без effort обязан нести сразу
// thinking.type=enabled и reasoning_effort по умолчанию (иначе апстрим deepseek-v4-flash цепочку не включает).
// Ступень по умолчанию = запасное "high" официального клиента, а при переданных supportedEfforts падает по конвейеру понижения до легальной ступени.
func TestInjectThinkingDefaultEffort(t *testing.T) {
	// Голый запрос без мыслительных параметров → инжектим enabled + reasoning_effort="high".
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
		false, nil)
	typ, present := getThinkingType(t, out)
	if !present || typ != "enabled" {
		t.Fatalf("thinking.type=%q present=%v want enabled (out=%s)", typ, present, out)
	}
	eff, ok := objFieldString(t, out, "reasoning_effort")
	if !ok || eff != "high" {
		t.Errorf("reasoning_effort=%q ok=%v, хотим high (ступень по умолчанию) (out=%s)", eff, ok, out)
	}

	// Модель поддерживает только low/high → high по умолчанию после конвейера понижения остаётся high (легальная ступень).
	out = PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`),
		false, map[string][]string{"deepseek-v4-flash": {"low", "high"}})
	eff, _ = objFieldString(t, out, "reasoning_effort")
	if eff != "high" {
		t.Errorf("при supportedEfforts=[low high] ступень по умолчанию=%q, хотим high (out=%s)", eff, out)
	}

	// Модель поддерживает только minimal/low → high по умолчанию понижаем до low (высшая поддерживаемая ≤high).
	out = PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`),
		false, map[string][]string{"deepseek-v4-flash": {"minimal", "low"}})
	eff, _ = objFieldString(t, out, "reasoning_effort")
	if eff != "low" {
		t.Errorf("при supportedEfforts=[minimal low] понижение ступени по умолчанию=%q, хотим low (out=%s)", eff, out)
	}

	// Явный enabled + отсутствующий effort → так же добиваем ступень по умолчанию (поведение официального configure thinking).
	out = PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"messages":[]}`),
		false, nil)
	if eff, _ = objFieldString(t, out, "reasoning_effort"); eff != "high" {
		t.Errorf("явный enabled без effort должен добить ступень по умолчанию, получили %q (out=%s)", eff, out)
	}
}

// TestInjectThinkingEffortNotOverridden — имеющийся явный reasoning_effort (snake/camel)
// никогда не перекрываем, не понижаем, не удаляем; понижением занимается отдельно normalizeReasoningEffort.
func TestInjectThinkingEffortNotOverridden(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"snake effort сохраняем как есть",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"medium","messages":[]}`},
		{"camel effort сохраняем как есть",
			`{"model":"deepseek-v4-flash","reasoningEffort":"low","messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, _ := getThinkingType(t, out)
			if typ != "enabled" {
				t.Fatalf("thinking.type=%q want enabled (out=%s)", typ, out)
			}
			// Ветка camel: на входе только camel, injectThinking не должен добавлять snake по умолчанию.
			if strings.Contains(c.body, "reasoningEffort") {
				if _, ok := objFieldString(t, out, "reasoning_effort"); ok {
					t.Errorf("при имеющемся reasoningEffort вдруг добавлен reasoning_effort по умолчанию (out=%s)", out)
				}
			}
		})
	}
	// Явный effort + нет thinking → инжектим enabled, но effort не перекрываем.
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","reasoning_effort":"medium","messages":[]}`),
		false, nil)
	if eff, _ := objFieldString(t, out, "reasoning_effort"); eff != "medium" {
		t.Errorf("явный effort переписали %q (out=%s)", eff, out)
	}
}

// TestInjectThinkingDisabledNoDefaultEffort — disabled сохраняет имеющуюся семантику:
// thinking.type=disabled уважает намерение выключить; reasoning_effort удаляем; ступень по умолчанию больше не добиваем.
func TestInjectThinkingDisabledNoDefaultEffort(t *testing.T) {
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`),
		false, nil)
	typ, present := getThinkingType(t, out)
	if !present || typ != "disabled" {
		t.Fatalf("thinking.type=%q present=%v want disabled (out=%s)", typ, present, out)
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := objFieldString(t, out, k); ok {
			t.Errorf("%s должен быть удалён и без добивки по умолчанию (при disabled) (out=%s)", k, out)
		}
	}
}

// objFieldString извлекает строковое значение поля верхнего уровня.
func objFieldString(t *testing.T, out []byte, key string) (string, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s, ok := m[key].(string)
	return s, ok
}

// TestInjectThinkingDeepSeekExplicitControl — явное управление клиентом обязаны уважать:
// непустой thinking.type (и enabled, и disabled) нельзя перекрывать.
func TestInjectThinkingDeepSeekExplicitControl(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantTyp string
		wantEff string // ожидаемое значение reasoning_effort; "" при wantEffAbsent=true означает, что поле должно быть удалено
	}{
		{"уже есть enabled не трогаем",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"messages":[]}`,
			"enabled", ""},
		{"уже есть enabled с reasoning_effort сохраняем (effort не удаляем)",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"high","messages":[]}`,
			"enabled", "high"},
		{"уже есть disabled сохраняем (уважаем намерение выключить)",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[]}`,
			"disabled", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present {
				t.Fatalf("поле thinking отсутствует (out=%s)", out)
			}
			if typ != c.wantTyp {
				t.Errorf("thinking.type = %q want %q (out=%s)", typ, c.wantTyp, out)
			}
			if c.wantEff != "" {
				if eff, _ := objFieldString(t, out, "reasoning_effort"); eff != c.wantEff {
					t.Errorf("reasoning_effort = %q want %q (out=%s)", eff, c.wantEff, out)
				}
			}
		})
	}
}

// TestInjectThinkingDisabledDeletesEffort — при явном disabled reasoning_effort заодно удаляем
// (копируем поведение case "deepseek" официального клиента: ветка по умолчанию delete reasoning_effort).
// Обе нотации snake/camel удаляем.
func TestInjectThinkingDisabledDeletesEffort(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"disabled + snake effort удаляем",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`},
		{"disabled + camel effort удаляем",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoningEffort":"high","messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present || typ != "disabled" {
				t.Fatalf("thinking.type = %q present=%v want disabled (out=%s)", typ, present, out)
			}
			for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
				if _, ok := objFieldString(t, out, k); ok {
					t.Errorf("%s должен быть удалён (при явном disabled) (out=%s)", k, out)
				}
			}
		})
	}
}

// TestInjectThinkingSkipNonDeepSeek — не-deepseek модель без изменений: без thinking ничего с нуля не добавляем,
// имеющийся thinking сохраняем как есть.
func TestInjectThinkingSkipNonDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"glm без thinking не инжектим", `{"model":"glm-5.2","messages":[]}`},
		{"glm с имеющимся thinking сохраняем", `{"model":"glm-5.2","thinking":{"type":"enabled"},"messages":[]}`},
		{"kimi без thinking не инжектим", `{"model":"kimi-k2.5","messages":[]}`},
		{"ветка qwen не инжектим", `{"model":"qwen2.5-coder-32b","messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Запоминаем, был ли thinking на входе и с каким значением, — на выходе семантика обязана побайтово сохраниться.
			var in map[string]any
			if err := json.Unmarshal([]byte(c.body), &in); err != nil {
				t.Fatalf("unmarshal input: %v", err)
			}
			inTh, inHadThink := in["thinking"]
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal out: %v (out=%s)", err, out)
			}
			outTh, outHadThink := got["thinking"]
			if inHadThink != outHadThink {
				t.Errorf("появление thinking изменилось: in=%v(had=%v) out=%v(had=%v)",
					inTh, inHadThink, outTh, outHadThink)
			}
			if inHadThink {
				// Сохранённый thinking обязан совпасть по исходному значению (на уровне семантики JSON).
				inJSON, _ := json.Marshal(inTh)
				outJSON, _ := json.Marshal(outTh)
				if string(inJSON) != string(outJSON) {
					t.Errorf("thinking изменили: in=%s out=%s", inJSON, outJSON)
				}
			}
			// Не-deepseek не должен обрастать прочими полями (кроме давно имеющегося принудительного stream).
			if len(got) != len(in)+1 {
				for k := range got {
					if _, had := in[k]; !had {
						t.Errorf("не-deepseek оброс полем %q (out=%s)", k, out)
					}
				}
			}
		})
	}
}

// TestInjectThinkingStringPreserved — инжект не должен ломать уже имеющиеся поля model/messages и т.п.
func TestInjectThinkingStringPreserved(t *testing.T) {
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`),
		false, nil)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["model"] != "deepseek-v4-flash" || m["temperature"] != 0.7 {
		t.Errorf("имеющиеся поля изменили: %v", m)
	}
	if _, ok := m["messages"].([]any); !ok {
		t.Errorf("структуру messages повредили: %v", m)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Errorf("stream не принудительный: %s", out)
	}
}
