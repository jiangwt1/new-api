package codex

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type Adaptor struct {
}

// toResponsesCallID converts a Claude tool call ID to Responses API format.
// Responses API expects IDs starting with "fc_".
func toResponsesCallID(id string) string {
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, "fc_") {
		return id
	}
	return "fc_" + id
}

// normalizeToolParameters ensures tool parameters have a properties field for object schemas.
func normalizeToolParameters(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 || string(schema) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(schema, &m); err != nil {
		return schema
	}

	typ, ok := m["type"]
	if !ok || string(typ) != `"object"` {
		return schema
	}

	if _, ok := m["properties"]; ok {
		return schema
	}

	m["properties"] = json.RawMessage(`{}`)
	out, err := json.Marshal(m)
	if err != nil {
		return schema
	}
	return out
}

// convertClaudeToolsToResponses converts Claude tools to Responses API format.
func convertClaudeToolsToResponses(tools any) (json.RawMessage, error) {
	if tools == nil {
		return nil, nil
	}

	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}

	var claudeTools []map[string]any
	if err := json.Unmarshal(toolsJSON, &claudeTools); err != nil {
		return nil, err
	}

	if len(claudeTools) == 0 {
		return nil, nil
	}

	var out []map[string]any
	for _, t := range claudeTools {
		toolType, _ := t["type"].(string)
		if strings.HasPrefix(toolType, "web_search") {
			out = append(out, map[string]any{"type": "web_search"})
			continue
		}

		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		var params json.RawMessage
		if p, ok := t["input_schema"]; ok {
			pj, _ := json.Marshal(p)
			params = normalizeToolParameters(pj)
		} else if p, ok := t["parameters"]; ok {
			pj, _ := json.Marshal(p)
			params = normalizeToolParameters(pj)
		} else {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}

		out = append(out, map[string]any{
			"type":        "function",
			"name":        name,
			"description": desc,
			"parameters":  params,
		})
	}

	return json.Marshal(out)
}

// convertClaudeToolChoiceToResponses converts Claude tool_choice to Responses API format.
func convertClaudeToolChoiceToResponses(toolChoice any) (json.RawMessage, error) {
	if toolChoice == nil {
		return nil, nil
	}

	tcJSON, err := json.Marshal(toolChoice)
	if err != nil {
		return nil, err
	}

	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(tcJSON, &tc); err != nil {
		return nil, err
	}

	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "none":
		return json.Marshal("none")
	case "tool":
		return json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		})
	default:
		return tcJSON, nil
	}
}

// extractToolResultOutput extracts text from a tool_result content field.
func extractToolResultOutput(content any) (string, []map[string]any) {
	if content == nil {
		return "(empty)", nil
	}

	if s, ok := content.(string); ok {
		if s == "" {
			return "(empty)", nil
		}
		return s, nil
	}

	blocks, ok := content.([]any)
	if !ok {
		return "(empty)", nil
	}

	var textParts []string
	var imageParts []map[string]any
	for _, block := range blocks {
		m, ok := block.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := m["type"].(string)
		switch blockType {
		case "text":
			if text, ok := m["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		case "image":
			src, _ := m["source"].(map[string]any)
			if src != nil {
				mediaType, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				if mediaType == "" {
					mediaType = "image/png"
				}
				if data != "" {
					imageParts = append(imageParts, map[string]any{
						"type":      "input_image",
						"image_url": "data:" + mediaType + ";base64," + data,
					})
				}
			}
		}
	}

	text := strings.Join(textParts, "\n\n")
	if text == "" {
		text = "(empty)"
	}
	return text, imageParts
}

// convertClaudeAssistantMessage converts an assistant message with potential tool_use blocks.
func convertClaudeAssistantMessage(msg dto.ClaudeMessage) ([]map[string]any, error) {
	var items []map[string]any

	if msg.IsStringContent() {
		content := msg.GetStringContent()
		if content != "" {
			parts := []map[string]any{{"type": "output_text", "text": content}}
			partsJSON, _ := json.Marshal(parts)
			items = append(items, map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": partsJSON,
			})
		}
		return items, nil
	}

	blocks, err := msg.ParseContent()
	if err != nil || len(blocks) == 0 {
		return items, nil
	}

	var textParts []string
	var toolUseBlocks []dto.ClaudeMediaMessage
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != nil && *block.Text != "" {
				textParts = append(textParts, *block.Text)
			}
		case "tool_use":
			toolUseBlocks = append(toolUseBlocks, block)
		}
	}

	if len(textParts) > 0 {
		parts := []map[string]any{}
		for _, t := range textParts {
			parts = append(parts, map[string]any{"type": "output_text", "text": t})
		}
		partsJSON, _ := json.Marshal(parts)
		items = append(items, map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": partsJSON,
		})
	}

	for _, block := range toolUseBlocks {
		args := "{}"
		if block.Input != nil {
			if inputJSON, err := json.Marshal(block.Input); err == nil {
				args = string(inputJSON)
			}
		}
		items = append(items, map[string]any{
			"type":      "function_call",
			"call_id":   toResponsesCallID(block.Id),
			"name":      block.Name,
			"arguments": args,
		})
	}

	return items, nil
}

// convertClaudeUserMessage converts a user message with potential tool_result blocks.
func convertClaudeUserMessage(msg dto.ClaudeMessage) ([]map[string]any, error) {
	var items []map[string]any

	if msg.IsStringContent() {
		content := msg.GetStringContent()
		if content != "" {
			items = append(items, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": content,
			})
		}
		return items, nil
	}

	blocks, err := msg.ParseContent()
	if err != nil || len(blocks) == 0 {
		return items, nil
	}

	var toolResultImageParts []map[string]any

	// Extract tool_result blocks -> function_call_output items
	for _, block := range blocks {
		if block.Type != "tool_result" {
			continue
		}
		outputText, imageParts := extractToolResultOutput(block.Content)
		items = append(items, map[string]any{
			"type":    "function_call_output",
			"call_id": toResponsesCallID(block.ToolUseId),
			"output":  outputText,
		})
		toolResultImageParts = append(toolResultImageParts, imageParts...)
	}

	// Collect remaining text and image blocks into user message
	var parts []map[string]any
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != nil && *block.Text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": *block.Text})
			}
		case "image":
			if block.Source != nil {
				dataStr, _ := block.Source.Data.(string)
				if dataStr != "" {
					mediaType := block.Source.MediaType
					if mediaType == "" {
						mediaType = "image/png"
					}
					parts = append(parts, map[string]any{
						"type":      "input_image",
						"image_url": "data:" + mediaType + ";base64," + dataStr,
					})
				}
			}
		}
	}
	parts = append(parts, toolResultImageParts...)

	if len(parts) > 0 {
		partsJSON, _ := json.Marshal(parts)
		items = append(items, map[string]any{
			"type":    "message",
			"role":    "user",
			"content": partsJSON,
		})
	}

	return items, nil
}

func (a *Adaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	info.RelayMode = relayconstant.RelayModeResponses
	info.FinalRequestRelayFormat = types.RelayFormatOpenAIResponses
	info.IsStream = true

	responsesReq := &dto.OpenAIResponsesRequest{
		Model:  request.Model,
		Stream: func(b bool) *bool { return &b }(true),
	}

	// Convert system to instructions
	if request.System != nil {
		if request.IsStringSystem() {
			systemText := request.GetStringSystem()
			if systemText != "" {
				responsesReq.Instructions = json.RawMessage(strconv.Quote(systemText))
			}
		} else {
			systemMedia := request.ParseSystem()
			if len(systemMedia) > 0 {
				var sb strings.Builder
				for i, media := range systemMedia {
					if media.Type == "text" && media.Text != nil {
						if i > 0 {
							sb.WriteString("\n")
						}
						sb.WriteString(*media.Text)
					}
				}
				if sb.Len() > 0 {
					responsesReq.Instructions = json.RawMessage(strconv.Quote(sb.String()))
				}
			}
		}
	}

	if len(responsesReq.Instructions) == 0 {
		responsesReq.Instructions = json.RawMessage(`"You are a helpful coding assistant."`)
	}

	// Convert messages to input items with tool support
	input := make([]any, 0)
	for _, msg := range request.Messages {
		var items []map[string]any
		var err error
		switch msg.Role {
		case "assistant":
			items, err = convertClaudeAssistantMessage(msg)
		case "user":
			items, err = convertClaudeUserMessage(msg)
		default:
			// Fallback: treat as plain text
			contentStr := extractTextContent(msg)
			if contentStr != "" {
				items = []map[string]any{{
					"type":    "message",
					"role":    msg.Role,
					"content": contentStr,
				}}
			}
		}
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			input = append(input, item)
		}
	}
	if b, err := common.Marshal(input); err == nil {
		responsesReq.Input = b
	}

	// Convert tools
	if request.Tools != nil {
		toolsJSON, err := convertClaudeToolsToResponses(request.Tools)
		if err != nil {
			return nil, err
		}
		responsesReq.Tools = toolsJSON
	}

	// Convert tool_choice
	if request.ToolChoice != nil {
		tcJSON, err := convertClaudeToolChoiceToResponses(request.ToolChoice)
		if err != nil {
			return nil, err
		}
		responsesReq.ToolChoice = tcJSON
	}

	// Convert reasoning effort from output_config
	effort := "high"
	if request.OutputConfig != nil {
		var cfg struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(request.OutputConfig, &cfg); err == nil && cfg.Effort != "" {
			effort = cfg.Effort
		}
	}
	if effort == "max" {
		effort = "xhigh"
	}
	responsesReq.Reasoning = &dto.Reasoning{
		Effort:  effort,
		Summary: "auto",
	}

	responsesReq.Store = json.RawMessage("false")

	return responsesReq, nil
}

// extractTextContent extracts plain text from Claude message content (fallback)
func extractTextContent(msg dto.ClaudeMessage) string {
	if msg.IsStringContent() {
		return msg.GetStringContent()
	}
	content, _ := msg.ParseContent()
	var sb strings.Builder
	for _, media := range content {
		if media.Type == "text" && media.Text != nil {
			sb.WriteString(*media.Text)
		}
	}
	return sb.String()
}

// extractOpenAIMessageContent extracts plain text from OpenAI Message content
func extractOpenAIMessageContent(msg dto.Message) string {
	if msg.IsStringContent() {
		return msg.StringContent()
	}
	content := msg.ParseContent()
	var sb strings.Builder
	for _, media := range content {
		if media.Type == "text" && media.Text != "" {
			sb.WriteString(media.Text)
		}
	}
	return sb.String()
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	info.RelayMode = relayconstant.RelayModeResponses
	info.FinalRequestRelayFormat = types.RelayFormatOpenAIResponses
	info.IsStream = true

	responsesReq := &dto.OpenAIResponsesRequest{
		Model:  request.Model,
		Stream: func(b bool) *bool { return &b }(true),
	}

	// Convert system prompt to instructions
	if len(request.Messages) > 0 {
		for _, msg := range request.Messages {
			if msg.Role == request.GetSystemRoleName() {
				if msg.IsStringContent() {
					systemText := msg.StringContent()
					if systemText != "" {
						responsesReq.Instructions = json.RawMessage(strconv.Quote(systemText))
					}
				} else {
					content := msg.ParseContent()
					var sb strings.Builder
					for _, media := range content {
						if media.Type == "text" && media.Text != "" {
							sb.WriteString(media.Text)
						}
					}
					if sb.Len() > 0 {
						responsesReq.Instructions = json.RawMessage(strconv.Quote(sb.String()))
					}
				}
				break
			}
		}
	}

	if len(responsesReq.Instructions) == 0 {
		responsesReq.Instructions = json.RawMessage(`""`)
	}

	// Convert messages to input
	input := make([]any, 0)
	for _, msg := range request.Messages {
		if msg.Role == request.GetSystemRoleName() {
			continue
		}
		contentStr := extractOpenAIMessageContent(msg)
		input = append(input, map[string]any{
			"type":    "message",
			"role":    msg.Role,
			"content": contentStr,
		})
	}
	if b, err := common.Marshal(input); err == nil {
		responsesReq.Input = b
	}

	responsesReq.Store = json.RawMessage("false")
	if request.Stream != nil {
		responsesReq.Stream = request.Stream
	}

	return responsesReq, nil
}

func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/rerank endpoint not supported")
}

func (a *Adaptor) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/embeddings endpoint not supported")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	isCompact := info != nil && info.RelayMode == relayconstant.RelayModeResponsesCompact

	if info != nil && info.ChannelSetting.SystemPrompt != "" {
		systemPrompt := info.ChannelSetting.SystemPrompt

		if len(request.Instructions) == 0 {
			if b, err := common.Marshal(systemPrompt); err == nil {
				request.Instructions = b
			} else {
				return nil, err
			}
		} else if info.ChannelSetting.SystemPromptOverride {
			var existing string
			if err := common.Unmarshal(request.Instructions, &existing); err == nil {
				existing = strings.TrimSpace(existing)
				if existing == "" {
					if b, err := common.Marshal(systemPrompt); err == nil {
						request.Instructions = b
					} else {
						return nil, err
					}
				} else {
					if b, err := common.Marshal(systemPrompt + "\n" + existing); err == nil {
						request.Instructions = b
					} else {
						return nil, err
					}
				}
			} else {
				if b, err := common.Marshal(systemPrompt); err == nil {
					request.Instructions = b
				} else {
					return nil, err
				}
			}
		}
	}
	if len(request.Instructions) == 0 {
		request.Instructions = json.RawMessage(`""`)
	}

	if isCompact {
		return request, nil
	}
	request.Store = json.RawMessage("false")
	request.MaxOutputTokens = nil
	request.Temperature = nil
	return request, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	if info.RelayMode != relayconstant.RelayModeResponses && info.RelayMode != relayconstant.RelayModeResponsesCompact {
		return nil, types.NewError(errors.New("codex channel: endpoint not supported"), types.ErrorCodeInvalidRequest)
	}

	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		return openai.OaiResponsesCompactionHandler(c, resp)
	}

	if info.RelayFormat == types.RelayFormatClaude {
		return openai.ResponsesToClaudeStreamHandler(c, info, resp)
	}

	if info.RelayFormat == types.RelayFormatOpenAI {
		if info.IsStream {
			return openai.OaiResponsesToChatStreamHandler(c, info, resp)
		}
		return openai.OaiResponsesToChatHandler(c, info, resp)
	}

	if info.IsStream {
		return openai.OaiResponsesStreamHandler(c, info, resp)
	}
	return openai.OaiResponsesHandler(c, info, resp)
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info.RelayMode != relayconstant.RelayModeResponses && info.RelayMode != relayconstant.RelayModeResponsesCompact {
		return "", errors.New("codex channel: only /v1/responses and /v1/responses/compact are supported")
	}
	path := "/backend-api/codex/responses"
	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		path = "/backend-api/codex/responses/compact"
	}
	return relaycommon.GetFullRequestURL(info.ChannelBaseUrl, path, info.ChannelType), nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, req)

	key := strings.TrimSpace(info.ApiKey)
	if !strings.HasPrefix(key, "{") {
		return errors.New("codex channel: key must be a JSON object")
	}

	oauthKey, err := ParseOAuthKey(key)
	if err != nil {
		return err
	}

	accessToken := strings.TrimSpace(oauthKey.AccessToken)
	accountID := strings.TrimSpace(oauthKey.AccountID)

	if accessToken == "" {
		return errors.New("codex channel: access_token is required")
	}
	if accountID == "" {
		return errors.New("codex channel: account_id is required")
	}

	req.Set("Authorization", "Bearer "+accessToken)
	req.Set("chatgpt-account-id", accountID)

	if req.Get("OpenAI-Beta") == "" {
		req.Set("OpenAI-Beta", "responses=experimental")
	}
	if req.Get("originator") == "" {
		req.Set("originator", "codex_cli_rs")
	}

	req.Set("Content-Type", "application/json")
	if info.IsStream {
		req.Set("Accept", "text/event-stream")
	} else if req.Get("Accept") == "" {
		req.Set("Accept", "application/json")
	}

	return nil
}
