// payload.go переписывает исходящее chat-тело для апстрима:
//  1. принудительно stream:true (апстрим отклоняет непотоковые)
//  2. нормализация tool_choice (у апстрима это поле string, форма объекта даст 400 code=11101)
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// PrepareBodyOpt — переписывание за один проход; при sanitize=false поведение полностью как было (только принудительный stream + нормализация tool_choice).
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEfforts(src, sanitize, nil)
}

// PrepareBodyOptWithEfforts — поверх PrepareBodyOpt даунгрейдит reasoning_effort по supportedEfforts модели:
// только если запрос явно несёт ступень, а модель её не поддерживает, — меняем на высшую поддерживаемую ≤ запрошенной; если все поддерживаемые выше запрошенной — берём низшую;
// неизвестная модель/неизвестная ступень/поле не несёт — всегда пропускаем. efforts равен nil — неизвестно (без даунгрейда).
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	normalizeToolChoice(obj)
	normalizeRoles(obj)
	// Переключатель цепочки рассуждений DeepSeek (см. thinking.go): внедряем thinking.type=enabled + добиваем ступень по умолчанию при отсутствии.
	// Выполняем раньше normalizeReasoningEffort: добитая ступень по умолчанию тоже идёт по существующему конвейеру даунгрейда,
	// если модель ступень по умолчанию не поддерживает — автоматически падает на высшую поддерживаемую ≤ ступени по умолчанию (не шлём наружу несоответствующую ступень).
	injectThinking(obj)
	normalizeReasoningEffort(obj, efforts)
	// Мног roundовая консистентность DeepSeek: если сообщения assistant несут следы reasoning — дописываем reasoning_content
	// (requiresReasoningContentOnAssistantMessages, см. thinking.go).
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// effortRank — ступени от низкой к высокой.
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort даунгрейдит reasoning_effort по supportedEfforts модели (совместимость обоих полей snake/camel).
//   - запрошенная ступень поддерживается моделью → пропускаем как есть
//   - запрошенная ступень не поддерживается → меняем на высшую поддерживаемую ≤ запрошенной (даунгрейд)
//   - все поддерживаемые выше запрошенной → берём низшую поддерживаемую (минимальное отклонение)
//   - неизвестная модель/неизвестная ступень/поле не несёт/модель не в кэше → всегда пропускаем
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// Среди поддерживаемых ≤ запрошенной выбираем высшую; переписываем, только если попали и отличается от запрошенной.
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("WARN: [upstream] reasoning_effort понижен model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// Все поддерживаемые выше запрошенной: берём низшую поддерживаемую.
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("WARN: [upstream] reasoning_effort приведён к минимуму model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles приводит роль developer в messages к system.
//
// Фон: апстрим проверяет поле role в messages по белому списку, developer в списке нет,
// попадание — сразу HTTP 400 code=11128. developer — псевдоним system в новом стандарте OpenAI
// (новые клиенты вроде Codex / Cursor несут им system-уровень инструкций), переписывание в system семантику не теряет.
//
// Эта нормализация — «совместимость протокола» (добивка белого списка role апстрима), а не «десенсибилизация содержимого»,
// потому намеренно отвязана от SanitizeFingerprints / параметра sanitize: даже при sanitize=false приводим как обычно.
//
// Признаём только это одно значение developer: остальные role (system/user/assistant/tool/любое неизвестное) всегда сохраняем как есть,
// не сливаем, не переупорядочиваем, не удаляем ни одного сообщения (поведение апстрима при нескольких system ещё не замерено, слияние внесло бы новую переменную).
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("[upstream] роль нормализована developer->system idx=%d", i)
		}
	}
}

// normalizeToolChoice переписывает OpenAI tool_choice под Go-struct апстрима (тип string).
//   - "none"            → удалить tool_choice + удалить tools/functions
//   - {"type":"none"}   → то же
//   - {"type":"auto"/"required"} → строка "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → строка "x"
//   - прочие объекты/нескаляры → удалить tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}
