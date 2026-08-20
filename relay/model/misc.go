package model

type Usage struct {
	PromptTokens        int `json:"prompt_tokens,omitempty"`
	CompletionTokens    int `json:"completion_tokens,omitempty"`
	TotalTokens         int `json:"total_tokens,omitempty"`
	PromptTokensDetails struct {
		CachedTokens     int `json:"cached_tokens,omitempty"`
		CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
		AudioTokens      int `json:"audio_tokens,omitempty"`
		TextTokens       int `json:"text_tokens,omitempty"`
		ImageTokens      int `json:"image_tokens,omitempty"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails struct {
		ReasoningTokens          int `json:"reasoning_tokens,omitempty"`
		AudioTokens              int `json:"audio_tokens,omitempty"`
		AcceptedPredictionTokens int `json:"accepted_prediction_tokens,omitempty"`
		RejectedPredictionTokens int `json:"rejected_prediction_tokens,omitempty"`
		TextTokens               int `json:"text_tokens,omitempty"`
		ImageTokens              int `json:"image_tokens,omitempty"`
	} `json:"completion_tokens_details,omitempty"`
}

type Error struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    any    `json:"code"`
	Status  any    `json:"status,omitempty"`
}

type ErrorWithStatusCode struct {
	Error
	StatusCode int `json:"status_code"`
}
type ClaudeError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}
