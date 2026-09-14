// Package prompt отдаёт собственные системные промпты шлюза: встроенный по умолчанию + перекрытие файлом + нейтральный промпт деградации.
//
// Контекст: клиенты (Claude Code/Codex и подобные CLI) инжектят в system prompt фиксированные шаблонные фразы,
// а контент-модерация апстрима бьёт по дословному точному совпадению и косит легитимный трафик (11128 из issue #36/PR39).
// Решение: шлюз перед отправкой заменяет клиентские system/developer сообщения собственным системным промптом,
// душа ложные срабатывания отпечатков из system в источнике (строки отпечатков в user/assistant сообщениях по-прежнему чистит
// internal/upstream/sanitize.go, два слоя складываются и друг друга не заменяют).
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded — промпт деградации: для обработки ложных срабатываний, нарочито предельно нейтральный.
//
// Сценарий срабатывания: в режиме passthrough запрос отклонён контент-политикой апстрима (HTTP 400 + текст модерации),
// после признания ложным срабатыванием отпечатка повторяем один раз с минимальным нейтральным промптом. Не каркас обхода — только чтобы обойти
// ложное срабатывание из system, легитимную семантику указаний пользователя не меняем.
const Degraded = "Ты — полезный помощник. Отвечай на языке пользователя, следуй указаниям пользователя, отвечай прямо и кратко."

// Load загружает текст системного промпта по mode и file.
//   - file непуст → читаем файл (нет/не читается — возвращаем error, вызывающий fails fast);
//   - file пуст → возвращаем встроенный defaultPrompt.
//
// mode здесь лишь транзитом фиксируем (реальную маршрутизацию custom/passthrough решает вызывающий),
// Load отвечает только за «добыть текст промпта», семантика маршрутизации его не касается.
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	return string(raw), nil
}

// Rewrite разбирает тело OpenAI-запроса и заменяет системный промпт:
//   - удаляет из messages все сообщения с role system/developer;
//   - в голову messages вставляем одно {"role":"system","content":systemPrompt};
//   - остальные поля и user/assistant/tool сообщения — побуквенно без изменений.
//
// Не разобрали → возвращаем как было (никогда не ошибаемся): Rewrite — критический путь исходящего переписывания,
// никакая ошибка разбора не должна стопить проксирование запроса, пусть апстрим разбирается по исходной семантике.
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// Нет поля messages или тип не тот → вставляем одно system и сохраняем остальные поля как были.
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// Выкидываем все system/developer сообщения, сохраняем user/assistant/tool и остальные роли.
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	// В голову вставляем одно system сообщение (prepend, чтобы не ломать семантику общего порядка).
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
