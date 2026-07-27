package constant

import "strings"

const (
	RelayModeUnknown = iota
	RelayModeChatCompletions
	RelayModeCompletions
	RelayModeEmbeddings
	RelayModeModerations
	RelayModeImagesGenerations
	RelayModeEdits
	RelayModeAudioSpeech
	RelayModeAudioTranscription
	RelayModeAudioTranslation
	RelayModeGeminiGenerateContent
	RelayModeGeminiStreamGenerateContent
	RelayModeClaude
	RelayModeOpenaiResponse
	RelayModeFlux
)

func Path2RelayMode(path string) int {
	relayMode := RelayModeUnknown
	// Gemini API 路径判断: /v1beta/models/{model}:{action} 或 /v1/models/{model}:{action}
	if strings.HasPrefix(path, "/v1beta/models/") || strings.HasPrefix(path, "/v1/models/") || strings.HasPrefix(path, "/v1alpha/models/") {
		if strings.Contains(path, ":generateContent") {
			relayMode = RelayModeGeminiGenerateContent
		} else if strings.Contains(path, ":streamGenerateContent") {
			relayMode = RelayModeGeminiStreamGenerateContent
		}
	} else if strings.HasPrefix(path, "/v1/chat/completions") {
		relayMode = RelayModeChatCompletions
	} else if strings.HasPrefix(path, "/v1/completions") {
		relayMode = RelayModeCompletions
	} else if strings.HasPrefix(path, "/v1/embeddings") {
		relayMode = RelayModeEmbeddings
	} else if strings.HasSuffix(path, "embeddings") {
		relayMode = RelayModeEmbeddings
	} else if strings.HasPrefix(path, "/v1/moderations") {
		relayMode = RelayModeModerations
	} else if strings.HasPrefix(path, "/v1/images/generations") || strings.HasPrefix(path, "/v1/images/edits") {
		relayMode = RelayModeImagesGenerations
	} else if strings.HasPrefix(path, "/v1/edits") {
		relayMode = RelayModeEdits
	} else if strings.HasPrefix(path, "/v1/audio/speech") {
		relayMode = RelayModeAudioSpeech
	} else if strings.HasPrefix(path, "/v1/audio/transcriptions") {
		relayMode = RelayModeAudioTranscription
	} else if strings.HasPrefix(path, "/v1/audio/translations") {
		relayMode = RelayModeAudioTranslation
	} else if strings.HasPrefix(path, "/v1/messages") {
		relayMode = RelayModeClaude
	} else if strings.HasPrefix(path, "/v1/responses") {
		relayMode = RelayModeOpenaiResponse
	} else if strings.HasPrefix(path, "/flux/v1/") {
		relayMode = RelayModeFlux
	}
	return relayMode
}

func Path2RelayModeGemini(path string) int {
	relayMode := RelayModeUnknown
	// 匹配 Gemini API 路径格式: /v1beta/models/{model}:{action}
	// 或 /v1/models/{model}:{action}
	if strings.Contains(path, ":generateContent") {
		relayMode = RelayModeGeminiGenerateContent
	} else if strings.Contains(path, ":streamGenerateContent") {
		relayMode = RelayModeGeminiStreamGenerateContent
	}
	return relayMode
}
