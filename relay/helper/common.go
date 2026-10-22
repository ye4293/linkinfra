package helper

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/channel/anthropic"
	"github.com/songquanpeng/one-api/relay/channel/openai"
)

func FlushWriter(c *gin.Context) error {
	if c.Writer == nil {
		return nil
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
		return nil
	}
	return errors.New("streaming error: flusher not found")
}

func ClaudeChunkData(c *gin.Context, resp anthropic.StreamResponse, data string) {
	c.Writer.Write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", resp.Type, data)))
	_ = FlushWriter(c)
}

func OpenaiResponseChunkData(c *gin.Context, resp openai.OpenaiResponseStreamResponse, data string) {
	if c.GetBool("responses_retry_diagnostics") {
		if !c.Writer.Written() {
			event := resp.Type[:min(len(resp.Type), 128)]
			c.Set("responses_diag_first_write", event)
			logger.Infof(c.Request.Context(), "responses_retry_diag event=first_write channel=%d source=upstream event_type=%q", c.GetInt("channel_id"), event)
		}
		switch resp.Type {
		case "response.created", "response.in_progress", "response.queued", "response.failed", "response.cancelled", "error":
		default:
			c.Set("responses_diag_non_prelude_events", c.GetInt("responses_diag_non_prelude_events")+1)
		}
		if resp.Type == "response.output_text.delta" {
			c.Set("responses_diag_text_deltas", c.GetInt("responses_diag_text_deltas")+1)
		}
	}
	c.Writer.Write([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", resp.Type, data)))
	_ = FlushWriter(c)
}

func StringData(c *gin.Context, str string) error {
	c.Render(-1, common.CustomEvent{Data: "data: " + str})
	_ = FlushWriter(c)
	return nil
}

func PingData(c *gin.Context) error {
	if c.GetBool("responses_retry_diagnostics") && !c.Writer.Written() {
		c.Set("responses_diag_first_write", "ping")
		logger.Infof(c.Request.Context(), "responses_retry_diag event=first_write channel=%d source=gateway event_type=ping", c.GetInt("channel_id"))
	}
	c.Writer.Write([]byte("event: ping\ndata: {\"type\": \"ping\"}\n\n"))
	_ = FlushWriter(c)
	return nil
}
