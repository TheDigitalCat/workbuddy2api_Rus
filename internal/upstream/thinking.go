// thinking.go — включение цепочки рассуждений DeepSeek: инжектим в исходящее тело thinking:{type:"enabled"} + ступень по умолчанию.
//
// Корневая причина (issue #43, подтверждено реверсом официального клиента codebuddy.js от Hermes):
// официальный клиент помечает deepseek-модели как thinkingFormat:"deepseek" + requiresReasoningContentOnAssistantMessages,
// для «включённого мышления» в запросе обязан явно лежать thinking:{type:"enabled"}, иначе апстрим по умолчанию отвечает без мышления
// (цепочку рассуждений не возвращает). Слой payload шлюза раньше вообще не знал этого поля, транзитные запросы шли без этого флага
// → апстрим цепочку не отдавал; glm/kimi идут по другим thinkingFormat (ветка qwen enable_thinking либо включено по умолчанию), потому у них всё штатно.
//
// Добивочный фикс (приёмочные замеры Hermes #43):
//
//	thinking.type=enabled одного поля мало — реальный апстрим deepseek-v4-flash на голый запрос «без reasoning_effort»
//	по-прежнему отвечает без мышления (длина reasoning_content 0), цепочка появляется только с reasoning_effort:high.
//	Реверс codebuddy.js подтверждает: isThinkingEnabled = !!(reasoning_summary || reasoning_effort || reasoning?.effort),
//	ветка enabled для case "deepseek" в реальном исходящем запросе сохраняет и reasoning_effort, официальное «включённое мышление» = thinking.type:enabled
//	+ какая-то ступень effort; ступень по умолчанию берётся из reasoning.defaultEffort ?? запасное "high" (при отсутствии источника configure thinking — warn fallback to 'high').
//
// Поведение выравниваем под официальный клиент (комбинация двух путей):
//   - thinking.type уже явно enabled / disabled → явное управление клиентом, никогда не перекрываем; при disabled копируем поведение case
//     удаляем reasoning_effort (оба поля snake/camel). При enabled без effort → добиваем ступень по умолчанию (поведение официального configure).
//   - нет thinking / пустой thinking.type / уже есть reasoning_effort → инжектим {type:"enabled"} + добиваем ступень по умолчанию.
//   - явный reasoning_effort никогда не перекрываем и не понижаем (понижение отдаём normalizeReasoningEffort в payload.go).
//   - не-deepseek модели (glm/kimi/qwen и др.) → без изменений.
package upstream

import (
	"strings"
)

// defaultDeepSeekEffort — запасная ступень официального клиента по умолчанию (при отсутствии источника configure thinking — warn fallback to 'high',
// в REASONING_SUPPLEMENTS.defaultEffort тоже "high"). После подстановки идёт по конвейеру понижения normalizeReasoningEffort,
// при неподдержке high моделью автоматически падает на высшую поддерживаемую ≤high.
const defaultDeepSeekEffort = "high"

// isDeepSeekModel — имя модели с префиксом deepseek (без учёта регистра).
// Накрывает варианты deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 и т.п.;
// префиксный матч выравниваем под критерий официального thinkingFormat:"deepseek", чтобы не пропустить инжект.
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// backfillReasoningContent — мультираундовая консистентность DeepSeek: если исторические сообщения assistant несут следы reasoning,
// апстрим требует, чтобы все сообщения assistant в следующих запросах несли поле reasoning_content (string, может быть пустой)
// — то есть requiresReasoningContentOnAssistantMessages (правило matches официального клиента).
//
// Правила (выравниваем под логику официального клиента):
//   - любое сообщение assistant в сессии с непустым reasoning (string) либо уже имеющимся полем reasoning_content
//     → все сообщения assistant обязаны иметь reasoning_content (string):
//   - непустой reasoning без reasoning_content → копируем значение reasoning
//   - уже есть reasoning_content → сохраняем как есть (не перекрываем)
//   - нет ни того ни другого → добиваем пустую строку ""
//   - ни у одного assistant нет следов reasoning → без изменений (зря поле не добавляем).
//
// Действует только для deepseek-моделей (thinkingFormat:deepseek + requiresReasoningContent).
func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	// Первый проход: выясняем, есть ли хоть какие-то следы reasoning (непустой reasoning либо уже имеющееся reasoning_content).
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !hasTrace {
		return
	}
	// Второй проход: всем сообщениям assistant добиваем/копируем поле reasoning_content.
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if _, ok := msg["reasoning_content"]; ok {
			continue // уже есть → не перекрываем
		}
		if r, ok := msg["reasoning"].(string); ok {
			msg["reasoning_content"] = r
		} else {
			msg["reasoning_content"] = ""
		}
	}
}

// injectThinking переписывает тело по правилам переключателя цепочки рассуждений DeepSeek. Не-deepseek — без изменений.
//
// Ядро логики (выравниваем под официальный клиент):
//   - «включённое мышление» обязано иметь thinking.type=enabled + ступень effort (добивочное доказательство Hermes #43).
//   - явный непустой thinking.type → явное управление клиентом: при enabled без effort добиваем ступень по умолчанию;
//     disabled уважаем и удаляем reasoning_effort (оба поля snake/camel).
//   - нет thinking / пустой type / уже есть effort → инжектим enabled и добиваем ступень по умолчанию (имеющийся effort не трогаем).
func injectThinking(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}
	// Ветка явного управления: непустой type (и enabled, и disabled — явное намерение) → type не меняем.
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return // disabled: мышление выключено и никакого effort (копируем поведение case клиента)
		}
		ensureDeepSeekEffort(obj) // явный enabled без effort → добиваем ступень по умолчанию
		return
	}
	// Нет thinking (или thinking — невалидное необъектное значение) либо у объекта thinking отсутствует/пуст type:
	// инжектим enabled (поведение case "deepseek" клиента). С имеющимся reasoning_effort тоже идём сюда
	// (effort оставляем существующей логике понижения, переключатель всё равно включаем).
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureDeepSeekEffort(obj)
}

// ensureDeepSeekEffort добивает ступень по умолчанию при отсутствующем effort (приоритет snake, запасной camel).
// Уже есть любой effort → не перекрываем (явную ступень никак не переписываем, понижение отдаём normalizeReasoningEffort).
func ensureDeepSeekEffort(obj map[string]any) {
	_, hasSnake := obj["reasoning_effort"]
	if hasSnake {
		return
	}
	_, hasCamel := obj["reasoningEffort"]
	if hasCamel {
		return
	}
	obj["reasoning_effort"] = defaultDeepSeekEffort
}
