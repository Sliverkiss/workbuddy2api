// report.go growth 域「对话活跃上报」接口：POST {billingBase}/v2/report。
// 照抄客户端 chat_request_send 事件形状（含 conversationId/mode/inputLength 等全字段，
// 勿用最小 3 字段，防上游后续加严）。事件必须带 userId（=账号 uid），缺失则服务端
// 200 但静默丢弃（实测见 REPORT-active-map.md §2）。
//
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务（领养前置）。
// 风控口径：每号每天 1 次即可（activity_hours 单时点），不做多时点高频上报。
package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// reportPath 活跃上报通道（实测）。
const reportPath = "/v2/report"

// chatRequestEvent 客户端 chat_request_send 事件完整形状（与 probe_active.py chat_event 对齐）。
// userId 为必填字段（= a.UID）；conversationId 由调用方生成，无需真实会话。
type chatRequestEvent struct {
	EventCode            string        `json:"eventCode"`
	Timestamp            int64         `json:"timestamp"`
	ReportDelay          int           `json:"reportDelay"`
	Mode                 string        `json:"mode"`
	ConversationID       string        `json:"conversationId"`
	RequestID            string        `json:"requestId"`
	InputLength          int           `json:"inputLength"`
	RequestModelID       string        `json:"requestModelId"`
	RequestModelName     string        `json:"requestModelName"`
	IsPlan               bool          `json:"isPlan"`
	IsAutoExecuteTerminal bool         `json:"isAutoExecuteTerminal"`
	IsAutoModify         bool          `json:"isAutoModify"`
	CodebaseEnable       bool          `json:"codebaseEnable"`
	MaxToken             int           `json:"maxToken"`
	MaxSteps             int           `json:"maxSteps"`
	Temperature          int           `json:"temperature"`
	MaxRetries           int           `json:"maxRetries"`
	MentionContexts      []any         `json:"mentionContexts"`
	KnowledgeID          []any         `json:"knowledgeId"`
	KnowledgeName        []any         `json:"knowledgeName"`
	CodebaseID           string        `json:"codebaseId"`
	MentionContextCount  int           `json:"mentionContextCount"`
	Command              string        `json:"command"`
	ExpertID             string        `json:"expertId"`
	RecommendID          string        `json:"recommendId"`
	SkillID              string        `json:"skillId"`
	SkillCount           int           `json:"skillCount"`
	TotalCount           int           `json:"totalCount"`
	FileURI              string        `json:"fileUri"`
	PresentAt            int64         `json:"presentAt"`
	TraceID              string        `json:"traceId"`
	RootRequestID        string        `json:"rootRequestId"`
	ParentConversationID string        `json:"parentConversationId"`
	AgentName            string        `json:"agentName"`
	AgentType            string        `json:"agentType"`
	UserID               string        `json:"userId"`
}

// ReportChatActivity 向上游发送一条对话活跃上报（chat_request_send）。
// conversationID 由调用方生成（如 wb2api-<ms>），无需真实会话——服务端不校验一致性。
// 错误语义与 doJSON 一致：HTTP 非 2xx / 业务 code != 0 → *Error。
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID string) error {
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode:            "chat_request_send",
		Timestamp:            now,
		ReportDelay:          0,
		Mode:                 "craft",
		ConversationID:       conversationID,
		RequestID:            conversationID,
		InputLength:          12,
		RequestModelID:       "deepseek-v4-flash",
		RequestModelName:     "DeepSeek V4 Flash",
		IsPlan:               false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:         false,
		CodebaseEnable:       false,
		MaxToken:             0,
		MaxSteps:             0,
		Temperature:          0,
		MaxRetries:           0,
		MentionContexts:      []any{},
		KnowledgeID:          []any{},
		KnowledgeName:        []any{},
		CodebaseID:           "",
		MentionContextCount:  0,
		Command:              "",
		ExpertID:             "",
		RecommendID:          "",
		SkillID:              "",
		SkillCount:           0,
		TotalCount:           0,
		FileURI:              "",
		PresentAt:            now,
		TraceID:              "",
		RootRequestID:        conversationID,
		ParentConversationID: conversationID,
		AgentName:            "default",
		AgentType:            "conversation",
		UserID:               a.UID,
	}
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.billingBase(a)+reportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	BillingHeaders(req, a)
	_, err = c.doJSON(req)
	return err
}
