package flux

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common/cloudflare"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// mirrorObjectPrefix R2 对象键前缀，所有 flux/replicate 转存图片都落在这个目录下
const mirrorObjectPrefix = "flux-images"

// MirrorResultURL 把上游临时图片 URL 转存到 R2，返回可长期访问的 R2 URL。
//
// BFL 与 Replicate 返回的图片链接都有时效（分别约 10 分钟 / 1 小时），过期后历史记录里
// 的图片全部失效，因此在落库前统一替换为 R2 URL。
//
// 以下情形原样返回 srcURL（降级，不影响任务成功判定）：
//   - srcURL 为空
//   - R2 配置不全，或未配置公共访问域 CfFilePublicUrl
//   - srcURL 已指向本端 R2（重复回调时保持幂等）
//   - 下载或上传失败（记 error 日志，图片仍可在有效期内访问）
//
// ctx 仅用于日志与取消传播的起点；内部会用 context.WithoutCancel 与调用方生命周期解耦，
// 避免 HTTP 客户端断开连接导致转存中断，同时套上 MirrorTotalBudget 总预算防止 goroutine 挂死。
func MirrorResultURL(ctx context.Context, taskID string, srcURL string) string {
	if srcURL == "" {
		return srcURL
	}
	if !cloudflare.IsConfigured() {
		return srcURL
	}
	// 没有公共访问域时转存产物是需要签名才能访问的 S3 Endpoint URL，
	// 替换掉上游可用的临时链接会制造永久死链，宁可不转存
	if !cloudflare.HasPublicURL() {
		logger.Warnf(ctx, "[flux-mirror] 未配置 R2 公共访问域（CfFilePublicUrl），跳过转存: task_id=%s", taskID)
		return srcURL
	}
	if cloudflare.IsR2URL(srcURL) {
		logger.Debugf(ctx, "[flux-mirror] 已是 R2 URL，跳过转存: task_id=%s", taskID)
		return srcURL
	}

	// WithoutCancel 只在这一层做一次：内部各阶段老实继承 mirrorCtx 的 deadline，
	// 总预算才是真生效的安全阀，而不是一个读起来像回事、实际拦不住任何操作的摆设。
	mirrorCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloudflare.MirrorTotalBudget)
	defer cancel()

	return mirrorOnce(mirrorCtx, ctx, taskID, srcURL)
}

// mirrorCall 表示一次进行中的转存，done 关闭后 url 可安全读取（空串表示失败）
type mirrorCall struct {
	done chan struct{}
	url  string
}

// mirrorInflight 按 task_id 去重进行中的转存
var mirrorInflight sync.Map

// mirrorOnce 保证同一 task 同时只有一次转存在跑，后到者等待并复用前者的结果。
//
// 同一张图会被多条路径同时处理：上游 webhook 因超时重投、对账器扫到尚未落库的记录、
// 客户端 from_source 轮询触发兜底。它们都在 CAS 落库之前，谁也拦不住谁——不去重的话
// 每条路径各下载上传一份完整图片，最终只有一个能 CAS 成功，其余全是没人引用、也没有
// 清理逻辑的 R2 孤儿对象，白白消耗带宽与存储。
//
// logCtx 仅用于日志；mirrorCtx 是带总预算的执行 context。
func mirrorOnce(mirrorCtx context.Context, logCtx context.Context, taskID string, srcURL string) string {
	// task_id 缺失时无法去重，直接转存（不能让空串把所有任务串成一条队列）
	if taskID == "" {
		return doMirror(mirrorCtx, logCtx, taskID, srcURL)
	}

	call := &mirrorCall{done: make(chan struct{})}
	if existing, loaded := mirrorInflight.LoadOrStore(taskID, call); loaded {
		other := existing.(*mirrorCall)
		logger.Infof(logCtx, "[flux-mirror] 同一任务已在转存中，等待复用其结果: task_id=%s", taskID)
		select {
		case <-other.done:
			if other.url != "" {
				return other.url
			}
			return srcURL // 前者失败了，一并降级
		case <-mirrorCtx.Done():
			logger.Warnf(logCtx, "[flux-mirror] 等待同任务转存超时，降级使用上游 URL: task_id=%s", taskID)
			return srcURL
		}
	}

	defer func() {
		mirrorInflight.Delete(taskID)
		close(call.done) // 必须在 url 赋值之后，close 同时是给等待方的内存同步点
	}()

	result := doMirror(mirrorCtx, logCtx, taskID, srcURL)
	if result != srcURL {
		call.url = result // 只有转存成功才让等待方复用；失败时留空串让它们各自降级
	}
	return result
}

// doMirror 执行一次真正的下载 + 上传，失败时返回原 srcURL（降级）
func doMirror(mirrorCtx context.Context, logCtx context.Context, taskID string, srcURL string) string {
	start := time.Now()
	r2URL, err := cloudflare.MirrorImageURLToR2(mirrorCtx, srcURL, mirrorObjectPrefix)
	if err != nil {
		logger.Errorf(logCtx, "[flux-mirror] 转存失败，降级使用上游临时 URL: task_id=%s, src=%s, err=%v",
			taskID, srcURL, err)
		return srcURL
	}

	logger.Infof(logCtx, "[flux-mirror] 转存成功: task_id=%s, cost=%dms, r2=%s",
		taskID, time.Since(start).Milliseconds(), r2URL)
	return r2URL
}

// BuildReadyResultJSON 组装 BFL query 格式的成功结果 JSON（{id,status:"Ready",result:{sample}}）。
// 同步、webhook、对账三条路径都要写这份 JSON 到 images.result，GetFlux 会原样返回给客户端，
// 因此必须收口在一处——三份手写副本一旦漂移，就会表现为"同一模型有时缺字段"的间歇问题。
func BuildReadyResultJSON(taskID string, imageURL string) string {
	resultBytes, err := json.Marshal(map[string]any{
		"id":     taskID,
		"status": "Ready",
		"result": map[string]any{"sample": imageURL},
	})
	if err != nil {
		return ""
	}
	return string(resultBytes)
}

// StoredSampleURL 返回 DB 中已落库的图片 URL（通常是转存后的 R2 URL），查不到时返回空串。
// 用于 from_source 查询路径：上游返回的永远是临时 URL，但库里可能已经是永久 URL，
// 不用库里的值会让同一个接口在 from_source 开关下返回两种寿命不同的链接。
func StoredSampleURL(taskID string) string {
	image, err := model.GetImageByTaskId(taskID)
	if err != nil || image == nil {
		return ""
	}
	return image.StoreUrl
}
