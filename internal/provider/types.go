package provider

// Request is the OpenAI Chat Completions shape, narrowed to the fields V1
// commits to. Streaming, tools, and multi-modal content are intentionally
// absent until proven needed; expanding this struct is an explicit decision.
type Request struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

type Response struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Error is an OpenAI-shaped error payload. Adapters return *Error so
// callers can branch on Type without sniffing strings.
type Error struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string {
	if e.Type != "" {
		return e.Type + ": " + e.Message
	}
	return e.Message
}
