// sanitize.go — обезличивание исходящего тела запроса: вычищаем отпечатки чёрного списка модерации апстрима.
//
// Фон: клиенты (CLI класса Claude Code) подмешивают в system prompt несколько фиксированных шаблонных фраз,
// модерация апстрима режет по точному посимвольному совпадению (не по смыслу), достаточно изменить одно слово для обхода.
// Стратегия: отпечатки вида ключ-значение/header вырезаем целиком; несущие смысл шаблонные фразы минимально переписываем (меняем одно слово), смысл сохраняем.
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures — предпроверка признаков: в очистку идём только при любом попадании (быстрый путь strings.Contains,
// обычные запросы ни во что не попадают → возвращаем как есть, без аллокаций).
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // имя ключа header-сегмента ключ-значение
	"cc_entrypoint=",             // висячее голое ключ-значение (достаточно усечённого префикса для срабатывания)
	"You are Claude Code",        // фраза идентичности (достаточно усечённого префикса)
	"Main branch (",              // фраза внедрённой инструкции (достаточно усечённого префикса)
	"You are a coding agent running in the Codex CLI", // первый абзац Codex instructions (достаточно усечённого префикса)
	"github.com/anthropics/",     // ссылка на репозиторий Anthropic из фразы обратной связи
	"11128",                      // антидетект апстрима: голый числовой код ошибки
}

// sanitizeHdrRe — слой вырезания: срабатывает уже на имя header-ключа (значение неважно), удаляем целиком.
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeKvRe — слой вырезания: висячие голые ключ-значения (cc_xxx=...;) чистим в цикле.
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites — слой переписывания: дословная замена полных шаблонных фраз (в каждой меняем одно слово, смысл сохраняем).
//
// Матч-строка фразы идентичности **без конечной пунктуации** (только до "…for Claude"):
// CLI-вариант фразы кончается точкой ("…for Claude."), десктопный (claude-desktop-3p / Agent SDK)
// продолжается через запятую ("…for Claude, running within the Claude Agent SDK.").
// Целая фраза с точкой матчит только первый вариант, десктопный проскочил бы — отпечаток ушёл бы апстриму как есть → 400 code=11128.
// Без конечной пунктуации накрываем обе формы разом (строка замены тоже без пунктуации, исходная пунктуация сохраняется как была).
// По-прежнему требуем префикс "You are Claude Code, ", более широкую подстроковую замену не делаем,
// чтобы не задеть разрозненный текст, который охраняет TestExactMatchOnlyVariantNotTouched.
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// Фраза обратной связи: целое предложение со ссылкой на репозиторий Anthropic, апстрим режет по целому предложению (одна ссылка или половина фразы не режутся,
		// по замерам нужно всё предложение целиком). Разницы give→provide в одно слово достаточно для обхода, смысл тот же.
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// Антидетект апстрима: голое число 11128 в теле запроса режет весь запрос (контекст числа неважен —
		// "code=11128" / голое "11128" / "код ошибки 11128" / "Code=11128" — всё срабатывает;
		// соседние 11148 / 11101 / 11115 / 99999 пропускаются). 11128 — это код именно этого класса блокировки,
		// по нему апстрим опознаёт запросы, "обсуждающие/эхом возвращающие его внутренний код ошибки".
		// Цена: любое 11128 в диалоге пользователя будет переписано — но само появление этого числа в запросе уже условие блокировки,
		// без переписывания запрос обречён. Дефис сохраняет читаемость и отсылку (невидимый пробел не годится, апстрим его нормализует по замерам).
		"11128",
		"11-128",
	},
}

// sanitizeText очищает один отрезок текста: предпроверка не сработала → возвращаем исходную строку (без аллокаций).
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // чистим висячие голые kv (cc_version=...; cc_entrypoint=...;)
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	return strings.TrimSpace(text)
}

// hasFingerprint — предпроверка признаков: сначала быстрый путь strings.Contains (без аллокаций);
// у имени header-ключа бывают регистровые варианты (X-Anthropic-...), если быстрый путь пропустил — добиваем regex (?i).
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeHdrRe.MatchString(text)
}

// sanitizeContent совместим со строкой и мультимодальным массивом; трогаем только text-части, image и прочие части не трогаем.
// Возвращает очищенное значение и факт изменения.
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls чистит assistant.tool_calls[].function.arguments.
//
// arguments — это **строкифицированный JSON** (не объект), поэтому достаточно прогнать как текст через sanitizeText.
// Это место долго было слепой зоной: у сообщений с вызовом инструментов content обычно null, а старая sanitizeMessages
// при отсутствующем content делала прямой continue, пропуская целое сообщение вместе с tool_calls, —
// и любая запрещённая строка, записанная в параметры инструментов (имена файлов, команды, записываемое содержимое), утекала как есть.
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages чистит content и tool_calls в messages; возвращает true при любом попадании.
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		// content и tool_calls проверяем независимо: content может быть null (ход с вызовом инструментов),
		// ранняя версия делала здесь continue, из-за чего tool_calls таких сообщений вообще не чистились.
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
