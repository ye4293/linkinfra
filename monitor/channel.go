package monitor

import (
	"fmt"
	"time"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/common/message"
	"github.com/songquanpeng/one-api/model"
)

func notifyRootUser(subject string, content string) {
	// 发送飞书通知
	if config.FeishuWebhookUrls != "" {
		err := message.SendFeishuNotification(subject, content)
		if err != nil {
			logger.SysError(fmt.Sprintf("failed to send feishu notification: %s", err.Error()))
		}
	}

	notifyRootUserWithoutFeishu(subject, content)
}

// notifyRootUserWithoutFeishu 发送通知（不包括飞书，用于避免重复发送）
func notifyRootUserWithoutFeishu(subject string, content string) {
	// 发送 MessagePusher 通知
	if config.MessagePusherAddress != "" {
		err := message.SendMessage(subject, content, content)
		if err != nil {
			logger.SysError(fmt.Sprintf("failed to send message: %s", err.Error()))
		} else {
			return
		}
	}

	// 发送邮件通知
	if config.RootUserEmail == "" {
		config.RootUserEmail = model.GetRootUserEmail()
	}
	err := message.SendEmail(subject, config.RootUserEmail, content)
	if err != nil {
		logger.SysError(fmt.Sprintf("failed to send email: %s", err.Error()))
	}
}

// DisableChannelSafely disable & notify with multi-key channel protection
func DisableChannelSafely(channelId int, channelName string, reason string, modelName string) {
	DisableChannelSafelyWithStatusCode(channelId, channelName, reason, modelName, 0)
}

// DisableChannelSafelyWithStatusCode disable & notify with multi-key channel protection, including status code
func DisableChannelSafelyWithStatusCode(channelId int, channelName string, reason string, modelName string, statusCode int) {
	// 检查渠道信息
	channel, err := model.GetChannelById(channelId, true)
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to get channel %d: %s", channelId, err.Error()))
		return
	}

	if channel.MultiKeyInfo.IsMultiKey {
		// 对于多key渠道，不应该直接禁用整个渠道
		// 记录警告信息，需要管理员手动处理
		logger.SysLog(fmt.Sprintf("Multi-key channel #%d (%s) has external issues: %s (状态码: %d). Not auto-disabling the entire channel as it may have working keys. Manual intervention may be required.",
			channelId, channelName, reason, statusCode))
		return
	}

	// 单key渠道使用内联逻辑，避免重复获取渠道信息
	disableChannelInternalWithStatusCode(channel, channelId, channelName, reason, modelName, statusCode)
}

// disableChannelInternal 内部禁用函数，接受已获取的channel对象
func disableChannelInternal(channel *model.Channel, channelId int, channelName string, reason string, modelName string) {
	disableChannelInternalWithStatusCode(channel, channelId, channelName, reason, modelName, 0)
}

// disableChannelInternalWithStatusCode 内部禁用函数，包含状态码
func disableChannelInternalWithStatusCode(channel *model.Channel, channelId int, channelName string, reason string, modelName string, statusCode int) {
	if !channel.AutoDisabled {
		logger.SysLog(fmt.Sprintf("channel #%d (%s) should be disabled but auto-disable is turned off, reason: %s", channelId, channelName, reason))
		return
	}

	disabled, err := model.AutoDisableChannelById(channelId, reason, modelName)
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to auto disable channel %d: %s", channelId, err.Error()))
		return
	}

	if !disabled {
		logger.SysLog(fmt.Sprintf("channel #%d (%s) auto disable skipped because it was already disabled or not eligible, reason: %s", channelId, channelName, reason))
		return
	}

	logger.SysLog(fmt.Sprintf("channel #%d has been disabled: %s", channelId, reason))

	// 发送飞书通知（带详细信息）
	if config.FeishuWebhookUrls != "" {
		err := message.SendFeishuChannelDisableNotification(channelId, channelName, statusCode, reason, modelName)
		if err != nil {
			logger.SysError(fmt.Sprintf("failed to send feishu channel disable notification: %s", err.Error()))
		}
	}

	// 发送邮件和其他通知
	subject := fmt.Sprintf("Channel \"%s\" (#%d) has been disabled", channelName, channelId)
	content := fmt.Sprintf(`
<h3>Channel Auto-Disabled</h3>
<p><strong>Channel name:</strong> %s</p>
<p><strong>Channel ID:</strong> #%d</p>
<p><strong>Model:</strong> %s</p>
<p><strong>Status code:</strong> %d</p>
<p><strong>Reason:</strong> %s</p>
<p><strong>Disabled at:</strong> %s</p>
<hr>
<p>This channel was automatically disabled due to errors. Please verify the channel configuration and API key validity.</p>
`, channelName, channelId, modelName, statusCode, reason, time.Now().Format("2006-01-02 15:04:05"))
	notifyRootUserWithoutFeishu(subject, content)
}

// DisableChannel disable & notify
func DisableChannel(channelId int, channelName string, reason string, modelName string) {
	DisableChannelWithStatusCode(channelId, channelName, reason, modelName, 0)
}

// DisableChannelWithStatusCode disable & notify, including status code
func DisableChannelWithStatusCode(channelId int, channelName string, reason string, modelName string, statusCode int) {
	// 检查渠道是否允许自动禁用
	channel, err := model.GetChannelById(channelId, true)
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to get channel %d: %s", channelId, err.Error()))
		return
	}

	disableChannelInternalWithStatusCode(channel, channelId, channelName, reason, modelName, statusCode)
}

func MetricDisableChannel(channelId int, successRate float64) {
	// 检查渠道是否允许自动禁用
	channel, err := model.GetChannelById(channelId, true)
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to get channel %d: %s", channelId, err.Error()))
		return
	}

	if !channel.AutoDisabled {
		logger.SysLog(fmt.Sprintf("channel #%d should be disabled due to low success rate %.2f%% but auto-disable is turned off", channelId, successRate*100))
		return
	}

	// 对于多key渠道，不应该基于整体成功率直接禁用整个渠道
	// 因为可能只是部分key有问题，应该让单个key的错误处理来决定
	if channel.MultiKeyInfo.IsMultiKey {
		logger.SysLog(fmt.Sprintf("Multi-key channel #%d has low success rate %.2f%%, but not auto-disabling the entire channel. Individual key errors will be handled separately. Manual review recommended.",
			channelId, successRate*100))

		// 发送通知但不禁用
		subject := fmt.Sprintf("Multi-key channel #%d has a low success rate", channelId)
		content := fmt.Sprintf("Multi-key channel #%d had a %.2f%% success rate over the last %d requests (threshold: %.2f%%). The channel was not auto-disabled because it has multiple keys — please check each key manually.",
			channelId, successRate*100, config.MetricQueueSize, config.MetricSuccessRateThreshold*100)
		notifyRootUser(subject, content)
		return
	}

	// 单key渠道使用禁用逻辑
	reason := fmt.Sprintf("success rate %.2f%% below threshold %.2f%%", successRate*100, config.MetricSuccessRateThreshold*100)
	modelName := "N/A (Metric)" // 成功率禁用没有特定的模型名称
	disableChannelInternal(channel, channelId, channel.Name, reason, modelName)
}

// EnableChannel enable & notify
func EnableChannel(channelId int, channelName string) {
	err := model.UpdateChannelStatusById(channelId, common.ChannelStatusEnabled)
	if err != nil {
		logger.SysError(fmt.Sprintf("Failed to enable channel %d: %s", channelId, err.Error()))
	}
	logger.SysLog(fmt.Sprintf("channel #%d has been enabled", channelId))
	subject := fmt.Sprintf("Channel \"%s\" (#%d) has been re-enabled", channelName, channelId)
	content := fmt.Sprintf("Channel \"%s\" (#%d) has been re-enabled.", channelName, channelId)
	notifyRootUser(subject, content)
}

// StartKeyNotificationListener 启动Key禁用通知监听器
func StartKeyNotificationListener() {
	// 启动Key级别的禁用通知监听器
	go func() {
		for notification := range model.KeyDisableNotificationChan {
			// 发送飞书通知（带详细信息）
			if config.FeishuWebhookUrls != "" {
				err := message.SendFeishuKeyDisableNotification(
					notification.ChannelId,
					notification.ChannelName,
					notification.KeyIndex,
					notification.MaskedKey,
					notification.StatusCode,
					notification.ErrorMessage,
				)
				if err != nil {
					logger.SysError(fmt.Sprintf("failed to send feishu key disable notification: %s", err.Error()))
				}
			}

			// 构建邮件主题和内容
			subject := fmt.Sprintf("Key disabled in channel \"%s\" (#%d)", notification.ChannelName, notification.ChannelId)
			content := fmt.Sprintf(`
<h3>Key Auto-Disabled</h3>
<p><strong>Channel name:</strong> %s</p>
<p><strong>Channel ID:</strong> #%d</p>
<p><strong>Disabled key:</strong> Key #%d (%s)</p>
<p><strong>Reason:</strong> %s</p>
<p><strong>Status code:</strong> %d</p>
<p><strong>Disabled at:</strong> %s</p>
<hr>
<p>This key was automatically disabled due to errors. If all keys are disabled, the channel will also be disabled.</p>
`, notification.ChannelName, notification.ChannelId, notification.KeyIndex, notification.MaskedKey,
				notification.ErrorMessage, notification.StatusCode, notification.DisabledTime.Format("2006-01-02 15:04:05"))

			// 发送邮件和其他通知（不包括飞书，避免重复发送）
			notifyRootUserWithoutFeishu(subject, content)
		}
	}()

	// 启动渠道级别的禁用通知监听器
	go func() {
		for notification := range model.ChannelDisableNotificationChan {
			// 发送飞书通知（带详细信息）
			if config.FeishuWebhookUrls != "" {
				err := message.SendFeishuChannelFullDisableNotification(
					notification.ChannelId,
					notification.ChannelName,
					notification.Reason,
				)
				if err != nil {
					logger.SysError(fmt.Sprintf("failed to send feishu channel full disable notification: %s", err.Error()))
				}
			}

			// 构建邮件主题和内容
			subject := fmt.Sprintf("Channel \"%s\" (#%d) fully disabled — all keys exhausted", notification.ChannelName, notification.ChannelId)
			content := fmt.Sprintf(`
<h3>Channel Fully Disabled</h3>
<p><strong>Channel name:</strong> %s</p>
<p><strong>Channel ID:</strong> #%d</p>
<p><strong>Reason:</strong> %s</p>
<p><strong>Disabled at:</strong> %s</p>
<hr>
<p>All keys in this channel have been disabled, so the channel has been automatically disabled. Please fix the key issues and re-enable the channel.</p>
`, notification.ChannelName, notification.ChannelId, notification.Reason, notification.DisabledTime.Format("2006-01-02 15:04:05"))

			// 发送邮件和其他通知（不包括飞书，避免重复发送）
			notifyRootUserWithoutFeishu(subject, content)
		}
	}()
}
