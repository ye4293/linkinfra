package service

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/tidwall/gjson"
)

const responsesStateKeyIndex = "responses_state_key_index"
const responsesStateKeyFingerprint = "responses_state_key_fingerprint"

func responsesKeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
}

func IsResponsesStatePath(path string) bool {
	p := strings.TrimRight(path, "/")
	return p == "/v1/responses" || p == "/v1/responses/compact"
}

func responsesStateKey(userID int, kind, value string) string {
	encoded, _ := json.Marshal([]any{userID, kind, value})
	return fmt.Sprintf("responses_state:v1:%x", sha256.Sum256(encoded))
}

type stateReference struct {
	key           string
	resourceBound bool
}

func inputStateReferences(userID int, body []byte) []stateReference {
	var refs []stateReference
	add := func(kind, value string, bound bool) {
		if value != "" {
			refs = append(refs, stateReference{responsesStateKey(userID, kind, value), bound})
		}
	}
	add("response", gjson.GetBytes(body, "previous_response_id").String(), true)
	conversation := gjson.GetBytes(body, "conversation")
	if conversation.Type == gjson.String {
		add("conversation", conversation.String(), true)
	} else {
		add("conversation", conversation.Get("id").String(), true)
	}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		switch item.Get("type").String() {
		case "reasoning", "compaction":
			if encrypted := item.Get("encrypted_content").String(); encrypted != "" {
				add("encrypted", encrypted, false)
			} else {
				// 没有可携带密文时，ID 依赖原上游保存的资源。
				add("item", item.Get("id").String(), true)
			}
		case "item_reference":
			add("item", item.Get("id").String(), true)
		}
	}
	return refs
}

// BindResponsesState 在首次选渠前恢复实际历史来源，不修改请求正文。
func BindResponsesState(c *gin.Context) error {
	return bindResponsesState(c, currentResponsesStateStore())
}

func bindResponsesState(c *gin.Context, store responsesStateStore) error {
	if !IsResponsesStatePath(c.Request.URL.Path) {
		return nil
	}
	body, err := common.GetRequestBody(c)
	if err != nil {
		return err
	}
	explicit := strings.ToLower(strings.TrimSpace(c.GetHeader("X-Linkinfra-Provider")))
	if len(explicit) > 128 {
		return errors.New("X-Linkinfra-Provider is too long")
	}
	refs := inputStateReferences(c.GetInt("id"), body)
	if len(refs) == 0 {
		if explicit != "" {
			c.Request = c.Request.WithContext(model.WithRetryProvider(c.Request.Context(), explicit))
		}
		return nil
	}
	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, ref.key)
	}
	origins, err := store.Lookup(c.Request.Context(), keys)
	if err != nil {
		return err
	}
	provider, haveProvider := explicit, explicit != ""
	channelID, keyIndex := 0, -1
	keyFingerprint := ""
	for _, ref := range refs {
		origin, found := origins[ref.key]
		if !found {
			if explicit != "" && !ref.resourceBound {
				continue
			}
			return errors.New("Responses history origin is unknown or expired; resume with its original X-Linkinfra-Provider, resend complete portable history, or start a new conversation")
		}
		if haveProvider && provider != origin.Provider {
			return errors.New("Responses history contains incompatible providers; refusing to discard thinking or compaction")
		}
		provider, haveProvider = origin.Provider, true
		if ref.resourceBound || origin.Provider == "" {
			if channelID != 0 && (channelID != origin.ChannelID || keyIndex != origin.KeyIndex || keyFingerprint != origin.KeyFingerprint) {
				return errors.New("Responses history references incompatible upstream resources")
			}
			channelID, keyIndex = origin.ChannelID, origin.KeyIndex
			keyFingerprint = origin.KeyFingerprint
		}
	}
	ctx := model.WithRetryProvider(c.Request.Context(), provider)
	if channelID > 0 {
		ctx = model.WithResponseStateChannel(ctx, channelID)
		c.Set(responsesStateKeyIndex, keyIndex)
		c.Set(responsesStateKeyFingerprint, keyFingerprint)
	}
	c.Request = c.Request.WithContext(ctx)
	return nil
}

// RestoreResponsesStateKey 禁止服务端引用静默换 key；索引失效时明确拒绝。
func RestoreResponsesStateKey(c *gin.Context, channel *model.Channel) error {
	if model.ResponseStateChannel(c.Request.Context()) <= 0 {
		return nil
	}
	index := c.GetInt(responsesStateKeyIndex)
	key, err := channel.GetKeyByIndex(index)
	if err != nil || key == "" {
		return errors.New("The original Responses state key is unavailable")
	}
	if expected := c.GetString(responsesStateKeyFingerprint); expected != "" && expected != responsesKeyFingerprint(key) {
		return errors.New("The original Responses state key has changed")
	}
	c.Set("cached_key_index", index)
	return nil
}

func VerifyResponsesStateKey(c *gin.Context) error {
	if model.ResponseStateChannel(c.Request.Context()) <= 0 {
		return nil
	}
	if c.GetInt("key_index") != c.GetInt(responsesStateKeyIndex) {
		return errors.New("Responses state cannot switch upstream keys")
	}
	if expected := c.GetString(responsesStateKeyFingerprint); expected != "" && expected != responsesKeyFingerprint(c.GetString("actual_key")) {
		return errors.New("Responses state cannot switch upstream keys")
	}
	return nil
}

// RememberResponsesState 在普通响应或 SSE 事件转发前记录来源。只存指纹和路由元数据。
func RememberResponsesState(c *gin.Context, raw []byte, stream bool) error {
	return rememberResponsesState(c, raw, stream, currentResponsesStateStore())
}

func rememberResponsesState(c *gin.Context, raw []byte, stream bool, store responsesStateStore) error {
	root := gjson.ParseBytes(raw)
	origin := responsesStateOrigin{Provider: model.RetryProvider(c.Request.Context()), ChannelID: c.GetInt("channel_id"), KeyIndex: c.GetInt("key_index")}
	origin.KeyFingerprint = responsesKeyFingerprint(c.GetString("actual_key"))
	if origin.ChannelID <= 0 {
		return errors.New("Missing Responses channel origin")
	}
	entries := make(map[string]responsesStateOrigin)
	add := func(kind, value string) {
		if value != "" {
			entries[responsesStateKey(c.GetInt("id"), kind, value)] = origin
		}
	}
	item := func(value gjson.Result) {
		add("item", value.Get("id").String())
		if kind := value.Get("type").String(); kind == "reasoning" || kind == "compaction" {
			add("encrypted", value.Get("encrypted_content").String())
		}
	}
	if stream {
		if root.Get("item").Exists() {
			item(root.Get("item"))
		}
		root = root.Get("response")
	}
	add("response", root.Get("id").String())
	conversation := root.Get("conversation")
	if conversation.Type == gjson.String {
		add("conversation", conversation.String())
	} else {
		add("conversation", conversation.Get("id").String())
	}
	for _, value := range root.Get("output").Array() {
		item(value)
	}
	if len(entries) == 0 {
		return nil
	}
	return store.Remember(c.Request.Context(), entries)
}
