// logging.go — табличный лог уровня запроса: по окончании каждого /v1/chat/completions печатаем одну строку в stdout.
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// chatSeq — сквозной порядковый номер запроса в процессе.
var chatSeq atomic.Int64

// chatLogEnabled — главный выключатель табличного лога чата. В проде всегда true;
// тестовый пакет через TestMain ставит false, гася шум в stdout; тесты, проверяющие строки вывода, временно включают через withChatLog (R5).
var chatLogEnabled = true

// chatStat — статистика одного chat-запроса для лога; handler вешает defer, строка падает на выходе запроса.
type chatStat struct {
	start  time.Time
	model  string
	mode   string // "stream" | "sync"
	uid    string // полный uid, показываем только первые 8 символов
	ttfb   time.Duration
	toks   int // <0 означает отсутствие usage → показываем "-"
	status int

	logged bool
}

// newChatStat строит объект статистики от момента входа запроса в handler; toks по умолчанию -1 (нет usage).
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done идемпотентно роняет одну строку табличного лога.
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks)
}

// chatStatsReader при сквозной передаче стрима выхватывает точное значение usage.completion_tokens из последнего SSE-кадра,
// а также фиксирует TTFB по первому data-кадру; сырые байты отдаются ниже по течению без изменений.
// Важно: никакой оценки по рунам, число токенов всегда берём из usage апстрима.
type chatStatsReader struct {
	br       *bufio.Reader
	start    time.Time
	ttfb     time.Duration
	seen     bool // первый data-кадр уже видели (TTFB пишем один раз)
	hasUsage bool // нёс ли последний кадр usage
	tokens   int
	pend     []byte // буфер прочитанных, но ещё не отданных строк
}

// newChatStatsReaderSince берёт since за точку отсчёта TTFB (обычно момент входа запроса в handler).
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB возвращает задержку прибытия первого data-кадра; без кадров — 0.
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens возвращает usage.completion_tokens последнего кадра и флаг наличия; без usage ok=false.
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// parseSSELine разбирает одну строку "data: {...}": по первому кадру пишет TTFB, при наличии usage берёт точный completion_tokens.
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Usage *struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
}

// Read возвращает сырые данные, попутно разбирая статистику TTFB/токенов.
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody достаёт поле model из JSON запроса, при отсутствии ставит "-".
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens извлекает usage.completion_tokens из ответа Aggregate; при отсутствии возвращает -1.
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix показывает только первые 8 символов uid; пустой uid показывает "-".
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow печатает одну строку табличного лога запроса (прямо в stdout, без префикса метки времени log).
// toks<0 означает отсутствие usage, показываем "-".
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
