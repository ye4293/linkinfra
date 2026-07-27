package image_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	img "github.com/songquanpeng/one-api/common/image"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "golang.org/x/image/webp"
)

type CountingReader struct {
	reader    io.Reader
	BytesRead int
}

func (r *CountingReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	r.BytesRead += n
	return n, err
}

// tinyWebPBase64 是一个 1x1 的有损 webp，34 字节，
// 广泛用于浏览器 webp 支持检测。
//
// webp 不像 jpeg/png/gif 那样能用标准库现场编码（golang.org/x/image/webp
// 只提供解码器），所以只能内嵌 fixture。代价是尺寸只有 1x1，断言强度弱于
// 其它三种格式——宽高颠倒这类 bug 由下面 jpeg/png/gif 的非正方形尺寸覆盖。
const tinyWebPBase64 = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="

// imageFixture 一份自包含的测试图片。
type imageFixture struct {
	name   string
	format string
	width  int
	height int
	data   []byte
}

// buildFixtures 现场生成测试图片，不依赖网络。
//
// 此前这些用例硬编码了 5 个 Wikimedia URL，其中
// 2560px-Gfp-wisconsin-madison-the-nature-boardwalk.jpg 因 Wikimedia
// 限制了可用缩略图尺寸而返回 HTTP 400（原图路径也已 404）。测试既没检查
// StatusCode，又用 assert.NoError 而非 require，于是把一个 HTML 错误页
// 喂给 image.Decode，再对 nil 结果调 img.Bounds() 触发 panic，
// 直接炸掉整个测试二进制、连同包内其它测试结果一起吞掉。
//
// 三种尺寸刻意各不相同且都不是正方形，这样宽高被颠倒时断言一定会失败。
func buildFixtures(t *testing.T) []imageFixture {
	t.Helper()

	webpData, err := base64.StdEncoding.DecodeString(tinyWebPBase64)
	require.NoError(t, err, "内嵌 webp fixture 必须是合法 base64")

	return []imageFixture{
		{name: "jpeg", format: "jpeg", width: 7, height: 11, data: encodeJPEG(t, 7, 11)},
		{name: "png", format: "png", width: 13, height: 5, data: encodePNG(t, 13, 5)},
		{name: "gif", format: "gif", width: 9, height: 4, data: encodeGIF(t, 9, 4)},
		{name: "webp", format: "webp", width: 1, height: 1, data: webpData},
	}
}

// newTestImage 造一张带渐变的图，避免全同色被编码器优化成异常小的体积。
func newTestImage(width, height int) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			m.Set(x, y, color.RGBA{
				R: uint8((x * 255) / max(width-1, 1)),
				G: uint8((y * 255) / max(height-1, 1)),
				B: 128,
				A: 255,
			})
		}
	}
	return m
}

func encodeJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, newTestImage(width, height), nil))
	return buf.Bytes()
}

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, newTestImage(width, height)))
	return buf.Bytes()
}

func encodeGIF(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, gif.Encode(&buf, newTestImage(width, height), nil))
	return buf.Bytes()
}

func TestDecode(t *testing.T) {
	for _, c := range buildFixtures(t) {
		t.Run("Decode:"+c.name, func(t *testing.T) {
			reader := &CountingReader{reader: bytes.NewReader(c.data)}
			// require 而非 assert：解码失败时 m 是 nil，
			// 继续调 m.Bounds() 会 panic 并中断整个测试二进制
			m, format, err := image.Decode(reader)
			require.NoError(t, err)
			size := m.Bounds().Size()
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.width, size.X)
			assert.Equal(t, c.height, size.Y)
			t.Logf("Bytes read: %d / %d", reader.BytesRead, len(c.data))
		})
	}

	for _, c := range buildFixtures(t) {
		t.Run("DecodeConfig:"+c.name, func(t *testing.T) {
			reader := &CountingReader{reader: bytes.NewReader(c.data)}
			config, format, err := image.DecodeConfig(reader)
			require.NoError(t, err)
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.width, config.Width)
			assert.Equal(t, c.height, config.Height)
			t.Logf("Bytes read: %d / %d", reader.BytesRead, len(c.data))
		})
	}
}

func TestBase64(t *testing.T) {
	for _, c := range buildFixtures(t) {
		t.Run("Decode:"+c.name, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(c.data)
			body := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
			reader := &CountingReader{reader: body}
			m, format, err := image.Decode(reader)
			require.NoError(t, err)
			size := m.Bounds().Size()
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.width, size.X)
			assert.Equal(t, c.height, size.Y)
			t.Logf("Bytes read: %d", reader.BytesRead)
		})
	}

	for _, c := range buildFixtures(t) {
		t.Run("DecodeConfig:"+c.name, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(c.data)
			body := base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
			reader := &CountingReader{reader: body}
			config, format, err := image.DecodeConfig(reader)
			require.NoError(t, err)
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.width, config.Width)
			assert.Equal(t, c.height, config.Height)
			t.Logf("Bytes read: %d", reader.BytesRead)
		})
	}
}

// TestGetImageSize 覆盖 URL 分支。用 httptest 起本地 server 而不是打真实
// 外网：既保住了对 IsImageUrl + GetImageSizeFromUrl 的覆盖，又不会因为
// 第三方站点改策略、删文件而失败。
func TestGetImageSize(t *testing.T) {
	fixtures := buildFixtures(t)

	byPath := make(map[string]imageFixture, len(fixtures))
	for _, c := range fixtures {
		byPath["/"+c.name] = c
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := byPath[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// 刻意不设 Content-Type：IsImageUrl 用 http.DetectContentType
		// 嗅探响应体前 512 字节，而不是读 header。不设置才能真正测到嗅探路径。
		_, _ = w.Write(c.data)
	}))
	defer server.Close()

	for _, c := range fixtures {
		t.Run(c.name, func(t *testing.T) {
			width, height, err := img.GetImageSize(server.URL + "/" + c.name)
			require.NoError(t, err)
			assert.Equal(t, c.width, width)
			assert.Equal(t, c.height, height)
		})
	}
}

func TestGetImageSizeFromBase64(t *testing.T) {
	for _, c := range buildFixtures(t) {
		t.Run(c.name, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(c.data)
			width, height, err := img.GetImageSizeFromBase64(encoded)
			require.NoError(t, err)
			assert.Equal(t, c.width, width)
			assert.Equal(t, c.height, height)
		})
	}
}
