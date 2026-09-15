package mimo

import "github.com/songquanpeng/one-api/relay/model"

// Defaults from https://mimo.mi.com/static/docs/api/chat/openai-api.md.
// Channels may also configure other model IDs available to their API key.
var ModelList = []string{
	"mimo-v2.5-pro",
	"mimo-v2.5",
}

var ModelDetails = []model.APIModel{
	{
		Name:        "mimo-v2.5-pro",
		Provider:    "MiMo",
		Description: "Xiaomi MiMo-V2.5-Pro",
		Tags:        []string{"mimo", "chat"},
		PriceType:   "pay-per-token",
	},
	{
		Name:        "mimo-v2.5",
		Provider:    "MiMo",
		Description: "Xiaomi MiMo-V2.5",
		Tags:        []string{"mimo", "chat"},
		PriceType:   "pay-per-token",
	},
}
