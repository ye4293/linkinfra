package model

// AdjustAudioQuota 保留音频调用入口，使用通用原子额度操作。
func AdjustAudioQuota(userID, tokenID int, delta int64, reserve bool) error {
	return AdjustRelayQuota(userID, tokenID, delta, reserve)
}
