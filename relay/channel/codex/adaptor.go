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
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

type Adaptor struct {
}

func (a *Adaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	// Ensure RelayMode is set to Responses for Codex channel
	info.RelayMode = relayconstant.RelayModeResponses
	// Set final request format to indicate conversion
	info.FinalRequestRelayFormat = types.RelayFormatOpenAIResponses
	// Codex requires streaming
	info.IsStream = true

	responsesReq := &dto.OpenAIResponsesRequest{
		Model:  request.Model,
		Stream: lo.ToPtr(true), // Codex requires stream=true
	}

	// Convert system to instructions (Codex requires string format)
	if request.System != nil {
		if request.IsStringSystem() {
			systemText := request.GetStringSystem()
			if systemText != "" {
				responsesReq.Instructions = json.RawMessage(strconv.Quote(systemText))
			}
		} else {
			// System is array of content blocks - extract text
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

	// Default instructions (Codex expects non-empty)
	if len(responsesReq.Instructions) == 0 {
		responsesReq.Instructions = json.RawMessage(`"You are a helpful coding assistant."`)
	}

	// Convert messages to input with proper type field
	if len(request.Messages) > 0 {
		input := make([]any, 0, len(request.Messages))
		for _, msg := range request.Messages {
			// Extract text content as string
			contentStr := extractTextContent(msg)
			input = append(input, map[string]any{
				"type":    "message",
				"role":    msg.Role,
				"content": contentStr,
			})
		}
		if b, err := common.Marshal(input); err == nil {
			responsesReq.Input = b
		}
	}

	// codex: store must be false
	responsesReq.Store = json.RawMessage("false")

	return responsesReq, nil
}

// extractTextContent extracts plain text from Claude message content
func extractTextContent(msg dto.ClaudeMessage) string {
	if msg.IsStringContent() {
		return msg.GetStringContent()
	}
	// Array content - extract text blocks only
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
	// Array content - extract text blocks only
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
	// Ensure RelayMode is set to Responses for Codex channel
	info.RelayMode = relayconstant.RelayModeResponses
	info.FinalRequestRelayFormat = types.RelayFormatOpenAIResponses
	info.IsStream = true

	responsesReq := &dto.OpenAIResponsesRequest{
		Model:  request.Model,
		Stream: lo.ToPtr(true), // Codex requires stream=true
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

	// Default instructions
	if len(responsesReq.Instructions) == 0 {
		responsesReq.Instructions = json.RawMessage(`""`)
	}

	// Convert messages to input
	input := make([]any, 0)
	for _, msg := range request.Messages {
		if msg.Role == request.GetSystemRoleName() {
			continue // skip system message
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

	// Copy other fields from original request
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
	// Codex backend requires the `instructions` field to be present.
	// Keep it consistent with Codex CLI behavior by defaulting to an empty string.
	if len(request.Instructions) == 0 {
		request.Instructions = json.RawMessage(`""`)
	}

	if isCompact {
		return request, nil
	}
	// codex: store must be false
	request.Store = json.RawMessage("false")
	// rm max_output_tokens
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

	// If original request was Claude format, convert response back to Claude SSE
	if info.RelayFormat == types.RelayFormatClaude {
		return openai.ResponsesToClaudeStreamHandler(c, info, resp)
	}

	// If original request was Chat Completions, convert response back to Chat format
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

	// chatgpt.com/backend-api/codex/responses is strict about Content-Type.
	// Clients may omit it or include parameters like `application/json; charset=utf-8`,
	// which can be rejected by the upstream. Force the exact media type.
	req.Set("Content-Type", "application/json")
	if info.IsStream {
		req.Set("Accept", "text/event-stream")
	} else if req.Get("Accept") == "" {
		req.Set("Accept", "application/json")
	}

	return nil
}