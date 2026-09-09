package aws

import (
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/relay/channel/anthropic"
	"github.com/songquanpeng/one-api/relay/channel/aws/utils"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

var _ utils.AwsAdapter = new(Adaptor)

type Adaptor struct {
}

func (a *Adaptor) ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}

	claudeReq := anthropic.ConvertRequest(*request)
	c.Set(ctxkey.RequestModel, request.Model)
	c.Set(ctxkey.ConvertedRequest, claudeReq)
	return claudeReq, nil
}

func (a *Adaptor) DoResponse(c *gin.Context, awsCli *bedrockruntime.Client, meta *util.RelayMeta) (usage *model.Usage, err *model.ErrorWithStatusCode) {
	// 原生请求重试可能切换渠道，必须刷新为当前渠道映射后的模型。
	_, isOpenAIFormat := c.Get(ctxkey.ConvertedRequest)
	if !isOpenAIFormat || c.GetString(ctxkey.RequestModel) == "" {
		modelName := meta.OriginModelName
		if meta.ActualModelName != "" {
			modelName = meta.ActualModelName
		}
		c.Set(ctxkey.RequestModel, modelName)
	}

	// 检查是否是 Claude Native 请求（没有 ConvertedRequest 说明是原生请求）
	if isOpenAIFormat {
		// OpenAI 格式请求，返回 OpenAI 兼容格式
		if meta.IsStream {
			err, usage = StreamHandler(c, awsCli, meta)
		} else {
			err, usage = Handler(c, awsCli, meta)
		}
	} else {
		// Claude Native 请求，返回 Claude 原生格式
		if meta.IsStream {
			err, usage = NativeStreamHandler(c, awsCli, meta)
		} else {
			err, usage = NativeHandler(c, awsCli, meta)
		}
	}
	return
}
