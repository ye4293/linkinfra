package typesafe

import (
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

var ModelList = []string{"jev-latest", "jev-preview", "jev-1.13.0"}

// Adaptor 提供渠道目录信息。原生调用由 RelaySystemOneHelper 负责，
// 不允许将决策模型转换为聊天或图像协议。
type Adaptor struct{ openai.Adaptor }

func (a *Adaptor) GetChannelName() string { return "typesafe" }
func (a *Adaptor) GetModelList() []string { return ModelList }
func (a *Adaptor) GetModelDetails() []model.APIModel {
	result := make([]model.APIModel, 0, len(ModelList))
	for _, name := range ModelList {
		result = append(result, model.APIModel{Name: name, Provider: "TypeSafe", Description: "TypeSafe System One structured decisions", Tags: []string{"typesafe", "systemone"}, PriceType: "pay-per-token"})
	}
	return result
}
func (a *Adaptor) ConvertRequest(*gin.Context, int, *model.GeneralOpenAIRequest) (any, error) {
	return nil, fmt.Errorf("TypeSafe requires POST /v1/systemone")
}
func (a *Adaptor) ConvertImageRequest(*model.ImageRequest) (any, error) {
	return nil, fmt.Errorf("TypeSafe requires POST /v1/systemone")
}
func (a *Adaptor) DoRequest(*gin.Context, *util.RelayMeta, io.Reader) (*http.Response, error) {
	return nil, fmt.Errorf("TypeSafe requires POST /v1/systemone")
}
