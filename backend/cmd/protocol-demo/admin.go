package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// envelope 是内部 API 统一响应壳 {Code,Message,RequestID,Data}。
type envelope struct {
	Code    int             `json:"Code"`
	Message string          `json:"Message"`
	Data    json.RawMessage `json:"Data"`
}

// gwClient 是访问网关管理端/网关端的 HTTP 客户端。
type gwClient struct {
	base string
	jwt  string
	hc   *http.Client
}

func newGWClient(base string) *gwClient {
	return &gwClient{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 60 * time.Second}}
}

func (c *gwClient) raw(method, path string, body any, headers map[string]string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// admin 发起一次携带 JWT 的管理端调用，返回解码后的统一壳。
func (c *gwClient) admin(method, path string, body any) (int, envelope, error) {
	status, raw, err := c.raw(method, path, body, map[string]string{"Authorization": "Bearer " + c.jwt})
	if err != nil {
		return status, envelope{}, err
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if status >= 400 || env.Code != 0 {
		return status, env, fmt.Errorf("%s %s -> HTTP %d Code %d: %s :: %s", method, path, status, env.Code, env.Message, sample(string(raw)))
	}
	return status, env, nil
}

func (c *gwClient) adminOK(method, path string, body any) (envelope, error) {
	_, env, err := c.admin(method, path, body)
	return env, err
}

// ===== 登录 / 用户 =====

func (c *gwClient) login(username, password string) error {
	status, raw, err := c.raw(http.MethodPost, "/api/v1/auth/login",
		map[string]any{"Username": username, "Password": password}, nil)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("login decode: %w (body=%s)", err, sample(string(raw)))
	}
	if status != http.StatusOK || env.Code != 0 {
		return fmt.Errorf("login failed: HTTP %d Code %d %s", status, env.Code, env.Message)
	}
	var data struct {
		Token string `json:"Token"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.Token == "" {
		return fmt.Errorf("login returned no token (body=%s)", sample(string(raw)))
	}
	c.jwt = data.Token
	return nil
}

func (c *gwClient) me() (adminID string, mustChange bool, err error) {
	status, raw, err := c.raw(http.MethodGet, "/api/v1/auth/me", nil, map[string]string{"Authorization": "Bearer " + c.jwt})
	if err != nil {
		return "", false, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", false, fmt.Errorf("me decode: %w (body=%s)", err, sample(string(raw)))
	}
	if status != http.StatusOK || env.Code != 0 {
		return "", false, fmt.Errorf("me failed: HTTP %d Code %d %s", status, env.Code, env.Message)
	}
	var data struct {
		User struct {
			ID                 string `json:"ID"`
			MustChangePassword bool   `json:"MustChangePassword"`
		} `json:"User"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", false, fmt.Errorf("me decode data: %w (body=%s)", err, sample(string(raw)))
	}
	return data.User.ID, data.User.MustChangePassword, nil
}

func (c *gwClient) recharge(adminID string, amount float64, password string) error {
	path := "/api/v1/admin/users/" + adminID + "/wallet/recharge"
	_, env, err := c.admin(http.MethodPost, path, map[string]any{
		"Amount": amount, "Remark": "protocol-demo setup", "Password": password,
	})
	if err != nil {
		return err
	}
	_ = env
	return nil
}

// ===== 对外模型 =====

type adminModel struct {
	ID           string `json:"ID"`
	ExternalName string `json:"ExternalName"`
	Enabled      bool   `json:"Enabled"`
}

func (c *gwClient) listModels() ([]adminModel, error) {
	env, err := c.adminOK(http.MethodGet, "/api/v1/admin/models", nil)
	if err != nil {
		return nil, err
	}
	var out []adminModel
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	return out, nil
}

func (c *gwClient) createModel(name string) (string, error) {
	env, err := c.adminOK(http.MethodPost, "/api/v1/admin/models", map[string]any{
		"ExternalName": name,
		"Enabled":      true,
		"SaleRates":    map[string]any{"Input": 1, "Output": 1, "CacheRead": 1},
	})
	if err != nil {
		return "", err
	}
	var data struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", err
	}
	return data.ID, nil
}

// ensureModel 幂等确保存在指定对外模型，返回其 ID。
func (c *gwClient) ensureModel(name string) (string, error) {
	models, err := c.listModels()
	if err != nil {
		return "", err
	}
	for _, m := range models {
		if m.ExternalName == name {
			return m.ID, nil
		}
	}
	return c.createModel(name)
}

// ===== 渠道 =====

type adminChannel struct {
	ID        string `json:"ID"`
	Name      string `json:"Name"`
	Protocol  string `json:"Protocol"`
	BaseURL   string `json:"BaseURL"`
	Priority  int    `json:"Priority"`
	DeletedAt any    `json:"DeletedAt"`
}

func (c *gwClient) listChannels() ([]adminChannel, error) {
	env, err := c.adminOK(http.MethodGet, "/api/v1/admin/channels", nil)
	if err != nil {
		return nil, err
	}
	var out []adminChannel
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return nil, fmt.Errorf("decode channels: %w", err)
	}
	return out, nil
}

func (c *gwClient) createChannel(name, protocol, baseURL string) (string, error) {
	env, err := c.adminOK(http.MethodPost, "/api/v1/admin/channels", map[string]any{
		"Name": name, "Protocol": protocol, "BaseURL": baseURL, "Priority": 100,
	})
	if err != nil {
		return "", err
	}
	var data struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", err
	}
	return data.ID, nil
}

func (c *gwClient) batchDeleteChannels(ids []string) error {
	_, env, err := c.admin(http.MethodPost, "/api/v1/admin/channels/batch-delete", map[string]any{"IDs": ids})
	_ = env
	return err
}

// ensureChannel 幂等确保存在某协议的渠道，返回渠道 ID。
func (c *gwClient) ensureChannel(name, protocol, baseURL string) (string, error) {
	chans, err := c.listChannels()
	if err != nil {
		return "", err
	}
	for _, ch := range chans {
		if ch.Name != name {
			continue
		}
		if ch.Protocol == protocol && strings.TrimRight(ch.BaseURL, "/") == strings.TrimRight(baseURL, "/") {
			return ch.ID, nil
		}
		// 名称在、但配置不符：删除后重建，避免历史残留干扰。
		if derr := c.batchDeleteChannels([]string{ch.ID}); derr != nil {
			return "", fmt.Errorf("delete stale channel %s: %w", name, derr)
		}
		break
	}
	return c.createChannel(name, protocol, baseURL)
}

// ===== 渠道密钥 =====

type adminKey struct {
	ID    string `json:"ID"`
	Name  string `json:"Name"`
	State string `json:"State"`
}

func (c *gwClient) listKeys(channelID string) ([]adminKey, error) {
	env, err := c.adminOK(http.MethodGet, "/api/v1/admin/channels/"+channelID+"/keys", nil)
	if err != nil {
		return nil, err
	}
	var out []adminKey
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return nil, fmt.Errorf("decode keys: %w", err)
	}
	return out, nil
}

func (c *gwClient) createKey(channelID, name, credential string) (string, error) {
	env, err := c.adminOK(http.MethodPost, "/api/v1/admin/channels/"+channelID+"/keys",
		map[string]any{"Name": name, "Credential": credential})
	if err != nil {
		return "", err
	}
	var data struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", err
	}
	return data.ID, nil
}

func (c *gwClient) setKeyState(channelID, keyID, action string) error {
	_, env, err := c.admin(http.MethodPost, "/api/v1/admin/channels/"+channelID+"/keys/"+keyID+"/state",
		map[string]any{"Action": action})
	_ = env
	return err
}

// ensureKey 幂等确保渠道存在正常状态的密钥 "k1"，返回其 ID。
func (c *gwClient) ensureKey(channelID string) (string, error) {
	keys, err := c.listKeys(channelID)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Name == "k1" {
			if k.State != "normal" {
				_ = c.setKeyState(channelID, k.ID, "normal")
			}
			return k.ID, nil
		}
	}
	return c.createKey(channelID, "k1", "sk-demo")
}

// ===== 渠道内部模型 =====

type adminChannelModel struct {
	ID              string `json:"ID"`
	InternalModelID string `json:"InternalModelID"`
	ExternalModelID string `json:"ExternalModelID"`
	State           string `json:"State"`
}

func (c *gwClient) listChannelModels(channelID string) ([]adminChannelModel, error) {
	env, err := c.adminOK(http.MethodGet, "/api/v1/admin/channels/"+channelID+"/models", nil)
	if err != nil {
		return nil, err
	}
	var out []adminChannelModel
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return nil, fmt.Errorf("decode channel models: %w", err)
	}
	return out, nil
}

func (c *gwClient) createChannelModel(channelID, internalID, externalID string) (string, error) {
	env, err := c.adminOK(http.MethodPost, "/api/v1/admin/channels/"+channelID+"/models", map[string]any{
		"InternalModelID": internalID,
		"ExternalModelID": externalID,
		"CostRates":       map[string]any{"Input": 1, "Output": 1, "CacheRead": 1},
	})
	if err != nil {
		return "", err
	}
	var data struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", err
	}
	return data.ID, nil
}

func (c *gwClient) deleteChannelModel(channelID, mid string) error {
	_, env, err := c.admin(http.MethodDelete, "/api/v1/admin/channels/"+channelID+"/models/"+mid, nil)
	_ = env
	return err
}

func (c *gwClient) setModelState(channelID, mid, action string) error {
	_, env, err := c.admin(http.MethodPut, "/api/v1/admin/channels/"+channelID+"/models/"+mid+"/state",
		map[string]any{"Action": action})
	_ = env
	return err
}

// ensureChannelModel 幂等确保渠道内存在绑定指定对外模型的内部模型行（internal="demo-internal"）。
func (c *gwClient) ensureChannelModel(channelID, internalID, externalID string) (string, error) {
	rows, err := c.listChannelModels(channelID)
	if err != nil {
		return "", err
	}
	for _, m := range rows {
		if m.InternalModelID != internalID {
			continue
		}
		if m.ExternalModelID == externalID {
			if m.State != "normal" {
				_ = c.setModelState(channelID, m.ID, "normal")
			}
			return m.ID, nil
		}
		// 绑定了错误的对外模型：删除重建。
		if derr := c.deleteChannelModel(channelID, m.ID); derr != nil {
			return "", fmt.Errorf("delete stale channel model: %w", derr)
		}
		break
	}
	return c.createChannelModel(channelID, internalID, externalID)
}

// ===== 网关调用令牌 =====

type adminToken struct {
	ID          string `json:"ID"`
	DisplayName string `json:"DisplayName"`
}

func (c *gwClient) listTokens(userID string) ([]adminToken, error) {
	env, err := c.adminOK(http.MethodGet, "/api/v1/admin/tokens?UserID="+userID, nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		List []adminToken `json:"List"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("decode tokens: %w", err)
	}
	return data.List, nil
}

func (c *gwClient) createToken(userID, display string) (string, error) {
	env, err := c.adminOK(http.MethodPost, "/api/v1/admin/tokens", map[string]any{
		"UserID": userID, "DisplayName": display,
	})
	if err != nil {
		return "", err
	}
	var data struct {
		Plain string `json:"Plain"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return "", err
	}
	if data.Plain == "" {
		return "", fmt.Errorf("token Plain empty")
	}
	return data.Plain, nil
}

func (c *gwClient) batchDeleteTokens(ids []string) error {
	_, env, err := c.admin(http.MethodPost, "/api/v1/admin/tokens/batch-delete", map[string]any{"IDs": ids})
	_ = env
	return err
}

// ensureToken 幂等：删除同名 "protocol-demo" 令牌后新建一个，返回明文。
func (c *gwClient) ensureToken(userID string) (string, error) {
	list, err := c.listTokens(userID)
	if err != nil {
		return "", err
	}
	var stale []string
	for _, t := range list {
		if t.DisplayName == demoTokenName {
			stale = append(stale, t.ID)
		}
	}
	if len(stale) > 0 {
		if derr := c.batchDeleteTokens(stale); derr != nil {
			return "", fmt.Errorf("delete stale tokens: %w", derr)
		}
	}
	return c.createToken(userID, demoTokenName)
}

const demoTokenName = "protocol-demo"