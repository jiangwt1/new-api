package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

// ResponsesToClaudeStreamHandler converts OpenAI Responses SSE stream to Claude SSE stream
func ResponsesToClaudeStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var usage = &dto.Usage{}
	state := newClaudeStreamState()

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		var event dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &event); err != nil {
			sr.Error(err)
			return
		}

		claudeEvents := convertResponsesEventToClaude(&event, state)
		for _, evt := range claudeEvents {
			evtJSON, err := json.Marshal(evt)
			if err != nil {
				sr.Error(err)
				return
			}
			// Write Claude SSE event
			c.Writer.Write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", evt.Type, evtJSON)))
			c.Writer.Flush()
		}
	})

	// Finalize stream if not already closed
	if !state.MessageStopSent {
		finalEvents := finalizeClaudeStream(state, usage)
		for _, evt := range finalEvents {
			evtJSON, _ := json.Marshal(evt)
			c.Writer.Write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", evt.Type, evtJSON)))
			c.Writer.Flush()
		}
	}

	return usage, nil
}

// claudeStreamState tracks state for converting Responses SSE to Claude SSE
type claudeStreamState struct {
	MessageStartSent   bool
	MessageStopSent    bool
	ContentBlockIndex int
	ContentBlockOpen  bool
	CurrentBlockType  string // "text" | "thinking" | "tool_use"

	OutputIndexToBlockIdx map[int]int

	ResponseID   string
	Model         string
	InputTokens   int
	OutputTokens  int
	CacheReadTokens int

	TextBuffer strings.Builder
}

func newClaudeStreamState() *claudeStreamState {
	return &claudeStreamState{
		OutputIndexToBlockIdx: make(map[int]int),
	}
}

// ClaudeStreamEvent represents a Claude SSE event
type ClaudeStreamEvent struct {
	Type         string                  `json:"type"`
	Index        *int                    `json:"index,omitempty"`
	Message      *ClaudeStreamMessage     `json:"message,omitempty"`
	ContentBlock *ClaudeStreamContentBlock `json:"content_block,omitempty"`
	Delta        *ClaudeStreamDelta      `json:"delta,omitempty"`
	Usage        *ClaudeStreamUsage      `json:"usage,omitempty"`
}

// ClaudeStreamMessage represents the message in message_start event
type ClaudeStreamMessage struct {
	ID      string              `json:"id,omitempty"`
	Type    string              `json:"type"`
	Role    string              `json:"role"`
	Content []json.RawMessage   `json:"content"`
	Model   string              `json:"model,omitempty"`
	Usage   *ClaudeStreamUsage  `json:"usage,omitempty"`
}

// ClaudeStreamContentBlock represents a content block
type ClaudeStreamContentBlock struct {
	Type     string                 `json:"type"`
	Index    *int                   `json:"index,omitempty"`
	ID       string                 `json:"id,omitempty"`
	Name     string                 `json:"name,omitempty"`
	Input    json.RawMessage        `json:"input,omitempty"`
	Thinking string                 `json:"thinking,omitempty"`
	Text     string                 `json:"text,omitempty"`
}

// ClaudeStreamDelta represents delta data
type ClaudeStreamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

// ClaudeStreamUsage represents usage information
type ClaudeStreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func convertResponsesEventToClaude(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	switch event.Type {
	case "response.created":
		return handleResponseCreated(event, state)
	case "response.output_item.added":
		return handleOutputItemAdded(event, state)
	case "response.output_text.delta":
		return handleOutputTextDelta(event, state)
	case "response.output_item.done":
		return handleOutputItemDone(event, state)
	case "response.function_call_arguments.delta":
		return handleFunctionCallArgumentsDelta(event, state)
	case "response.reasoning_summary_text.delta":
		return handleReasoningDelta(event, state)
	case "response.completed", "response.incomplete":
		return handleResponseCompleted(event, state)
	default:
		return nil
	}
}

func handleResponseCreated(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	if event.Response != nil {
		state.ResponseID = event.Response.ID
		if event.Response.Model != "" {
			state.Model = event.Response.Model
		}
	}

	if state.MessageStartSent {
		return nil
	}
	state.MessageStartSent = true

	return []ClaudeStreamEvent{
		{
			Type: "message_start",
			Message: &ClaudeStreamMessage{
				ID:      state.ResponseID,
				Type:    "message",
				Role:    "assistant",
				Content: []json.RawMessage{},
				Model:   state.Model,
				Usage: &ClaudeStreamUsage{
					InputTokens:  0,
					OutputTokens: 0,
				},
			},
		},
	}
}

func handleOutputItemAdded(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	if event.Item == nil {
		return nil
	}

	switch event.Item.Type {
	case "message":
		// Skip message items - we'll handle text through output_text.delta
		return nil
	case "function_call":
		return handleFunctionCallItemAdded(event, state)
	case "reasoning":
		return handleReasoningItemAdded(event, state)
	default:
		return nil
	}
}

func handleFunctionCallItemAdded(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	var events []ClaudeStreamEvent
	events = append(events, closeCurrentBlock(state)...)

	idx := state.ContentBlockIndex
	if event.OutputIndex != nil {
		state.OutputIndexToBlockIdx[*event.OutputIndex] = idx
	}
	state.ContentBlockOpen = true
	state.CurrentBlockType = "tool_use"

	callID := fixCallID(event.Item.CallId)

	return append(events, ClaudeStreamEvent{
		Type:  "content_block_start",
		Index: &idx,
		ContentBlock: &ClaudeStreamContentBlock{
			Type:  "tool_use",
			ID:    callID,
			Name:  event.Item.Name,
			Input: json.RawMessage("{}"),
		},
	})
}

func handleReasoningItemAdded(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	var events []ClaudeStreamEvent
	events = append(events, closeCurrentBlock(state)...)

	idx := state.ContentBlockIndex
	if event.OutputIndex != nil {
		state.OutputIndexToBlockIdx[*event.OutputIndex] = idx
	}
	state.ContentBlockOpen = true
	state.CurrentBlockType = "thinking"

	return append(events, ClaudeStreamEvent{
		Type:  "content_block_start",
		Index: &idx,
		ContentBlock: &ClaudeStreamContentBlock{
			Type:     "thinking",
			Thinking: "",
		},
	})
}

func handleOutputTextDelta(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	if event.Delta == "" {
		return nil
	}

	var events []ClaudeStreamEvent

	if !state.ContentBlockOpen || state.CurrentBlockType != "text" {
		events = append(events, closeCurrentBlock(state)...)

		idx := state.ContentBlockIndex
		state.ContentBlockOpen = true
		state.CurrentBlockType = "text"

		events = append(events, ClaudeStreamEvent{
			Type:  "content_block_start",
			Index: &idx,
			ContentBlock: &ClaudeStreamContentBlock{
				Type: "text",
				Text: "",
			},
		})
	}

	idx := state.ContentBlockIndex
	return append(events, ClaudeStreamEvent{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &ClaudeStreamDelta{
			Type: "text_delta",
			Text: event.Delta,
		},
	})
}

func handleOutputItemDone(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	return closeCurrentBlock(state)
}

func handleFunctionCallArgumentsDelta(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	if event.Delta == "" {
		return nil
	}

	if event.OutputIndex == nil {
		return nil
	}
	blockIdx, ok := state.OutputIndexToBlockIdx[*event.OutputIndex]
	if !ok {
		return nil
	}

	return []ClaudeStreamEvent{
		{
			Type:  "content_block_delta",
			Index: &blockIdx,
			Delta: &ClaudeStreamDelta{
				Type:        "input_json_delta",
				PartialJSON: event.Delta,
			},
		},
	}
}

func handleReasoningDelta(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	if event.Delta == "" {
		return nil
	}

	if event.OutputIndex == nil {
		return nil
	}
	blockIdx, ok := state.OutputIndexToBlockIdx[*event.OutputIndex]
	if !ok {
		return nil
	}

	return []ClaudeStreamEvent{
		{
			Type:  "content_block_delta",
			Index: &blockIdx,
			Delta: &ClaudeStreamDelta{
				Type:     "thinking_delta",
				Thinking: event.Delta,
			},
		},
	}
}

func handleResponseCompleted(event *dto.ResponsesStreamResponse, state *claudeStreamState) []ClaudeStreamEvent {
	if state.MessageStopSent {
		return nil
	}

	var events []ClaudeStreamEvent
	events = append(events, closeCurrentBlock(state)...)

	stopReason := "end_turn"
	if event.Response != nil {
		if event.Response.Usage != nil {
			state.InputTokens = event.Response.Usage.InputTokens
			state.OutputTokens = event.Response.Usage.OutputTokens
			if event.Response.Usage.InputTokensDetails != nil {
				state.CacheReadTokens = event.Response.Usage.InputTokensDetails.CachedTokens
			}
		}
		reasoning := string(event.Response.Status)
		if reasoning == "incomplete" || reasoning == `"incomplete"` {
			if event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reasoning == "max_output_tokens" {
				stopReason = "max_tokens"
			}
		}
	}

	events = append(events, ClaudeStreamEvent{
		Type: "message_delta",
		Delta: &ClaudeStreamDelta{
			StopReason: stopReason,
		},
		Usage: &ClaudeStreamUsage{
			InputTokens:  state.InputTokens,
			OutputTokens: state.OutputTokens,
		},
	}, ClaudeStreamEvent{
		Type: "message_stop",
	})
	state.MessageStopSent = true

	return events
}

func finalizeClaudeStream(state *claudeStreamState, usage *dto.Usage) []ClaudeStreamEvent {
	if !state.MessageStartSent || state.MessageStopSent {
		return nil
	}

	var events []ClaudeStreamEvent
	events = append(events, closeCurrentBlock(state)...)

	events = append(events, ClaudeStreamEvent{
		Type: "message_delta",
		Delta: &ClaudeStreamDelta{
			StopReason: "end_turn",
		},
		Usage: &ClaudeStreamUsage{
			InputTokens:  state.InputTokens,
			OutputTokens: state.OutputTokens,
		},
	}, ClaudeStreamEvent{
		Type: "message_stop",
	})

	usage.PromptTokens = state.InputTokens
	usage.OutputTokens = state.OutputTokens
	usage.TotalTokens = state.InputTokens + state.OutputTokens
	usage.PromptTokensDetails.CachedTokens = state.CacheReadTokens

	return events
}

func closeCurrentBlock(state *claudeStreamState) []ClaudeStreamEvent {
	if !state.ContentBlockOpen {
		return nil
	}
	idx := state.ContentBlockIndex
	state.ContentBlockOpen = false
	state.ContentBlockIndex++
	return []ClaudeStreamEvent{
		{
			Type:  "content_block_stop",
			Index: &idx,
		},
	}
}

// fixCallID converts Responses call ID format to Claude format
// Responses uses "call_xxx" format, Claude uses "toolu_xxx" format
func fixCallID(callID string) string {
	if callID == "" {
		return ""
	}
	// If already in Claude format, return as-is
	if strings.HasPrefix(callID, "toolu_") || strings.HasPrefix(callID, "fc_") {
		return callID
	}
	// Convert "call_xxx" to "toolu_xxx"
	if strings.HasPrefix(callID, "call_") {
		return "toolu_" + callID[5:]
	}
	// For other formats, add prefix
	return "toolu_" + callID
}
