package magpie

// adminclient 是 magpie 页面接口（/api/*，需页面口令）的最小客户端：
// 供「令牌 ↔ callerKey」同步与后续限额同步使用。页面接口不认网关 Bearer，
// 只认 ?k=<页面口令> 换来的会话 cookie，所以这里带 cookiejar 先登录。
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// PageAdmin 调 magpie 的页面接口。
type PageAdmin struct {
	base string
	key  string
	hc   *http.Client
}

// NewPageAdmin 构造；base 例如 http://magpie:3430，key 为页面口令（MAGPIE_WEB_KEY）。
func NewPageAdmin(base, key string, hc *http.Client) *PageAdmin {
	if hc == nil {
		jar, _ := cookiejar.New(nil)
		hc = &http.Client{Timeout: 20 * time.Second, Jar: jar}
	}
	return &PageAdmin{base: strings.TrimRight(base, "/"), key: strings.TrimSpace(key), hc: hc}
}

// login 用 ?k= 换会话 cookie（一次即可，jar 记住）。
func (a *PageAdmin) login(ctx context.Context) error {
	if a.key == "" {
		return fmt.Errorf("未配置 magpie 页面口令，无法调用页面接口")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/?k="+url.QueryEscape(a.key), nil)
	if err != nil {
		return err
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("magpie 页面登录失败：%w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return nil
}

func (a *PageAdmin) doJSON(ctx context.Context, method, path string, body any, out any) error {
	if err := a.login(ctx); err != nil {
		return err
	}
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, buf)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("magpie %s %s 返回 %d：%s", method, path, resp.StatusCode, truncate(string(raw), 200))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("解析 magpie %s 响应失败：%w", path, err)
		}
	}
	return nil
}

type callerKeyRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type callerKeysResp struct {
	Keys   []callerKeyRow `json:"keys"`
	Secret string         `json:"secret"`
}

func (a *PageAdmin) listCallerKeys(ctx context.Context) ([]callerKeyRow, error) {
	var r callerKeysResp
	if err := a.doJSON(ctx, http.MethodGet, "/api/caller-keys", nil, &r); err != nil {
		return nil, err
	}
	return r.Keys, nil
}

// EnsureCallerKey 确保存在名为 name 的 callerKey，且其明文为我们给定的 secret。
// 返回使用中的明文（就是我们传入的 secret；若名字已存在则轮换为它）。幂等。
func (a *PageAdmin) EnsureCallerKey(ctx context.Context, name, secret string) (string, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("name/secret 不能为空")
	}
	// 先 add-key（自带我们的 secret）；成功即返回。
	var add callerKeysResp
	err := a.doJSON(ctx, http.MethodPost, "/api/caller-keys/add-key", map[string]any{"name": name, "secret": secret}, &add)
	if err == nil {
		return secret, nil
	}
	// 已存在：找到 id 后 rotate-key 成我们的 secret。
	keys, lerr := a.listCallerKeys(ctx)
	if lerr != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Name == name {
			var rot callerKeysResp
			if rerr := a.doJSON(ctx, http.MethodPost, "/api/caller-keys/rotate-key", map[string]any{"key": k.ID, "name": name, "secret": secret}, &rot); rerr != nil {
				return "", rerr
			}
			return secret, nil
		}
	}
	return "", err
}

// DeleteCallerKey 按名字删除 callerKey；不存在视为成功。幂等。
func (a *PageAdmin) DeleteCallerKey(ctx context.Context, name string) error {
	keys, err := a.listCallerKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.Name == name {
			return a.doJSON(ctx, http.MethodPost, "/api/caller-keys/remove-key", map[string]any{"key": k.ID, "name": name}, nil)
		}
	}
	return nil
}

// RenameCallerKey 原地改名 callerKey：ID 与 secret 都不变（真机实测，
// UI Rename 按钮同款端点 POST /api/caller-keys/rename-key {key,name}）。
// 名字已是目标名视为成功（幂等）；旧名不存在时按「已是目标名」幂等通过，
// 避免「rename 之前旧名已被删」的竞态卡死回填。
func (a *PageAdmin) RenameCallerKey(ctx context.Context, oldName, newName string) error {
	if strings.TrimSpace(oldName) == "" || strings.TrimSpace(newName) == "" {
		return fmt.Errorf("old/new name 不能为空")
	}
	if oldName == newName {
		return nil
	}
	if err := a.checkKeyNameLimit(newName); err != nil {
		return err
	}
	keys, err := a.listCallerKeys(ctx)
	if err != nil {
		return err
	}
	var id string
	for _, k := range keys {
		if k.Name == newName {
			// 目标名已存在（同名可重复是 magpie 允许的形态）：以「目标名存在」为幂等成功。
			return nil
		}
		if k.Name == oldName {
			id = k.ID
		}
	}
	if id == "" {
		// 旧名在代理侧不存在：视为已改名（幂等；本地映射行由调用方推进）。
		return nil
	}
	return a.doJSON(ctx, http.MethodPost, "/api/caller-keys/rename-key", map[string]any{"key": id, "name": newName}, nil)
}

// checkKeyNameLimit 按 magpie 真机上限（120 字符）拦截，避免把必然失败的请求打到代理侧。
func (a *PageAdmin) checkKeyNameLimit(name string) error {
	if n := utf8.RuneCountInString(name); n > MagpieKeyNameLimit {
		return fmt.Errorf("callerKey 名字 %d 字符超过代理侧上限 %d", n, MagpieKeyNameLimit)
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}
