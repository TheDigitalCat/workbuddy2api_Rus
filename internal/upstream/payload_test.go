package upstream

import (
	"encoding/json"
	"testing"
)

// TestNormalizeRoles проверяет, что исходящее тело приводит роль developer к system.
// В белом списке role апстрима нет developer (псевдоним system из нового стандарта OpenAI),
// попадание — сразу HTTP 400 code=11128; здесь assert'им полный тракт через PrepareBodyOptWithEfforts.
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // ожидаемые role один-в-один к выходным messages; len — число сообщений
	}{
		{"developer переписываем в system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer с заглавной переписываем",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER капсом переписываем",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"окружающие пробелы переписываем после TrimSpace",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system сохраняем как есть",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user сохраняем как есть",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant сохраняем как есть",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool сохраняем как есть (неизвестность не повод переписывать)",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"без messages без паники, остальные поля целы",
			`{"model":"glm-5.2"}`, []string{}},
		{"пустой массив messages без паники",
			`{"messages":[]}`, []string{}},
		{"в смешанных сообщениях переписываем только developer",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"при sanitize=false всё равно приводим (отвязка от обезличивания)",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"необъектные элементы сообщений пропускаем, остальные обрабатываем штатно",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Весь проход с sanitize=false: проверяем, что приведение role не зависит от выключателя обезличивания содержимого (D4).
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// Извлекаем role из выходных messages (необъектные элементы пропускаем, без паники).
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("число role не совпало: получили %v (%d), хотим %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// При отсутствующих messages остальные поля обязаны сохраниться как есть (кроме принудительного stream).
	t.Run("без messages остальные поля целы", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("остальные поля изменили: %v", obj)
		}
	})
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // какое effort-поле обязано быть в выводе; пусто означает, что поля быть не должно
		wantVal string // ожидаемое значение
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}
