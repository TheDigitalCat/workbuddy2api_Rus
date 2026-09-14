// report.go — интерфейс «отчёта об активности диалога» growth-домена: POST {billingBase}/v2/report.
// В точности копируем форму события chat_request_send клиента (со всеми полями conversationId/mode/inputLength и т.д.,
// не сводим к минимальным 3 полям, чтобы не сломаться при будущем ужесточении апстрима). Событие обязано нести userId (= uid аккаунта), при отсутствии сервер
// отвечает 200, но молча отбрасывает (замерено, см. REPORT-active-map.md §2).
//
// Один отчёт разом зажигает growth-серию входов подряд + разблокирует задачу first_buddy (предусловие приюта).
// Риск-контур: достаточно 1 раза в сутки на номер (activity_hours одной точкой), высокочастотные многоточечные отчёты не шлём.
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// reportPath — канал отчёта об активности (замерен).
const reportPath = "/v2/report"

// billingJSON шлёт запрос billing-домена (billingBase, codebuddy.cn) и разбирает конверт; при body nil тело запроса не несём.
// Симметричен growthJSON из travel.go (growth-домен идёт через chatBase + BillingHeaders; billing-домен — через billingBase).
// Общий для billing-эндпоинтов report/checkin и т.п.: заголовки единообразно BillingHeaders, конверт и семантика ошибок как у doJSON.
func (c *Client) billingJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	return c.doJSON(req)
}

// chatRequestEvent — полная форма события chat_request_send клиента (выровнена под chat_event из probe_active.py).
// userId — обязательное поле (= a.UID); conversationId генерирует вызывающая сторона, реальная сессия не нужна.
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// ReportChatActivity отправляет апстриму один отчёт об активности диалога (chat_request_send).
// conversationID генерирует вызывающая сторона (например wb2api-<ms>), реальная сессия не нужна — сервер консистентность не проверяет.
// requestID — независимый идентификатор запроса этого хода (при многоходовых отчётах одной сессии каждый свой); при пустом откатываемся на conversationID.
// Семантика ошибок как у doJSON: HTTP не-2xx / бизнес-code != 0 → *Error.
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID, requestID string) error {
	if requestID == "" {
		requestID = conversationID
	}
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode:             "chat_request_send",
		Timestamp:             now,
		ReportDelay:           0,
		Mode:                  "craft",
		ConversationID:        conversationID,
		RequestID:             requestID,
		InputLength:           12,
		RequestModelID:        "deepseek-v4-flash",
		RequestModelName:      "DeepSeek V4 Flash",
		IsPlan:                false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:          false,
		CodebaseEnable:        false,
		MaxToken:              0,
		MaxSteps:              0,
		Temperature:           0,
		MaxRetries:            0,
		MentionContexts:       []any{},
		KnowledgeID:           []any{},
		KnowledgeName:         []any{},
		CodebaseID:            "",
		MentionContextCount:   0,
		Command:               "",
		ExpertID:              "",
		RecommendID:           "",
		SkillID:               "",
		SkillCount:            0,
		TotalCount:            0,
		FileURI:               "",
		PresentAt:             now,
		TraceID:               "",
		RootRequestID:         conversationID,
		ParentConversationID:  conversationID,
		AgentName:             "default",
		AgentType:             "conversation",
		UserID:                a.UID,
	}
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}
	_, err = c.billingJSON(a, http.MethodPost, reportPath, json.RawMessage(raw))
	return err
}
