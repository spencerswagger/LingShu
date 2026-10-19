package protocolconv

import (
	"encoding/json"

	"github.com/team/llmgateway/internal/domain/billing"
)

const (
	BlockText       = "text"
	BlockImage      = "image"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
	BlockThinking   = "thinking"
)

type ContentBlock struct {
	Kind      string
	Text      string
	ImageURL  string
	ImageB64  string
	MediaType string

	ToolUseID  string
	ToolName   string
	ToolInput  json.RawMessage
	ToolResult []ContentBlock
	IsError    bool

	Signature string
}

type Message struct {
	Role   string
	Blocks []ContentBlock
}

type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type ToolChoice struct {
	Mode string
	Name string
}

const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
	ToolChoiceTool     = "tool"
)

type CanonicalRequest struct {
	Model       string
	System      []ContentBlock
	Messages    []Message
	Tools       []Tool
	ToolChoice  *ToolChoice
	Temperature *float64
	TopP        *float64
	MaxTokens   *int64
	Stop        []string
	Stream      bool
	Metadata    map[string]string
	Extra       map[string]json.RawMessage
}

type CanonicalResponse struct {
	ID           string
	Model        string
	Role         string
	Blocks       []ContentBlock
	FinishReason string
	Usage        billing.Usage
	// CacheCreation 记录 Anthropic 口径的 cache_creation_input_tokens（已并入 Usage.Input）；
	// 非 Anthropic 来源为 0，供目标协议判断是否存在需提示的缓存写信息。
	CacheCreation int64
	Extra         map[string]json.RawMessage
}

const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventError             = "error"
	EventPing              = "ping"
)

const (
	DeltaText     = "text"
	DeltaThinking = "thinking"
	DeltaToolJSON = "tool_json"
)

type CanonicalEvent struct {
	Type       string
	Index      int
	BlockKind  string
	DeltaKind  string
	Text       string
	ToolUseID  string
	ToolName   string
	StopReason string
	Usage      billing.Usage
	HasUsage   bool
	ErrMessage string
}
