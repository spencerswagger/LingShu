package identity

import (
	"time"

	"github.com/mojocn/base64Captcha"
)

// Captcha 图形验证码服务：生成待校验的验证码图片，并一次性校验用户答案。
type Captcha interface {
	Generate() (id string, imageBase64 string, err error)
	Verify(id, answer string) bool
}

// ImageCaptcha 基于 github.com/mojocn/base64Captcha 的数字图形验证码实现。
// 答案明文只存在于 store 中，绝不通过接口对外返回。
type ImageCaptcha struct {
	c     *base64Captcha.Captcha
	store base64Captcha.Store
}

// NewImageCaptcha 构造数字图形验证码：80x240、4 位数字。
// store 容量 1024 条，超过即触发 GC 清理过期项；验证码 10 分钟过期。
func NewImageCaptcha() Captcha {
	store := base64Captcha.NewMemoryStore(1024, 10*time.Minute)
	return &ImageCaptcha{
		c:     base64Captcha.NewCaptcha(base64Captcha.NewDriverDigit(80, 240, 4, 0.7, 80), store),
		store: store,
	}
}

// Generate 生成验证码图片（base64 编码的 PNG）并返回对应 id。
// 明文答案由库内部存入 store，此处丢弃，不对外暴露。
func (c *ImageCaptcha) Generate() (id string, imageBase64 string, err error) {
	id, imageBase64, _, err = c.c.Generate()
	return id, imageBase64, err
}

// Verify 校验用户答案，校验成功即从 store 中删除该验证码，保证一次性使用。
func (c *ImageCaptcha) Verify(id, answer string) bool {
	return c.store.Verify(id, answer, true)
}
