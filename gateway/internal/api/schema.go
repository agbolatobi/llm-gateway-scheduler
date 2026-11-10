// Package api defines the gateway's supported text-only Chat Completions subset.
package api

// ChatRequest deliberately rejects fields outside this initial subset.
type ChatRequest struct {
	Model               string         `json:"model"`
	Messages            []Message      `json:"messages"`
	MaxCompletionTokens *int           `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int           `json:"max_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	Stream              bool           `json:"stream,omitempty"`
	StreamOptions       *StreamOptions `json:"stream_options,omitempty"`
	RequestClass        string         `json:"request_class,omitempty"` // Gateway extension.
}

type Message struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
	Name    string  `json:"name,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Completion and Chunk describe the supported text-only backend response shapes.
type Completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // chat.completion
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"` // chat.completion.chunk
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"` // null until generation finishes.
}

type Delta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ErrorResponse struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}
