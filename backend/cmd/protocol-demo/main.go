// Command protocol-demo 是一个自包含的「协议直通/转换」演示与测试夹具：
//  1. 起三个 mock 上游（openai-chat / openai-responses / anthropic-messages），
//     各自只讲一种协议、严格校验请求形状，收到异种协议形状返回 400；
//  2. 通过管理端 HTTP API 幂等地配置网关（三个对外模型 + 三条内联协议渠道 + 密钥 + 内部模型 + 一个调用令牌）；
//  3. 覆盖 3 入站协议 × 3 渠道 = 9 路径，各跑 8 个用例（text/tool/reason/image × 流式/非流式），
//     断言下游响应为入站协议形状且内容匹配，并断言上游确实收到目标协议形状；
//  4. 打印 PASS/FAIL 表，任一失败即非零退出。
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	var (
		gateway   = flag.String("gateway", "http://127.0.0.1:8080", "网关基址")
		adminUser = flag.String("admin-user", "admin", "管理员用户名")
		adminPass = flag.String("admin-password", "admin12345", "管理员密码")
	)
	flag.Parse()

	if err := run(*gateway, *adminUser, *adminPass); err != nil {
		fmt.Fprintf(os.Stderr, "\n%v\n", err)
		os.Exit(1)
	}
}

func run(base, adminUser, adminPass string) error {
	fmt.Printf("== protocol-demo ==\n网关: %s\n\n", base)

	// 1) 起三个 mock 上游。
	mocks := map[string]*mock{
		protoChat: newMock("mock-openai-chat", protoChat, "/chat/completions", mockChatAddr),
		protoResp: newMock("mock-openai-responses", protoResp, "/responses", mockRespAddr),
		protoAnth: newMock("mock-anthropic", protoAnth, "/v1/messages", mockAnthAddr),
	}
	var stops []func()
	for _, proto := range []string{protoChat, protoResp, protoAnth} {
		stop, err := mocks[proto].start()
		if err != nil {
			return err
		}
		stops = append(stops, stop)
	}
	defer func() {
		for _, s := range stops {
			s()
		}
	}()
	fmt.Printf("mock 上游已就绪: openai-chat=%s openai-responses=%s anthropic=%s\n\n",
		mockChatAddr, mockRespAddr, mockAnthAddr)

	// 2) 探测网关存活。
	hc := &http.Client{Timeout: 90 * time.Second}
	if err := healthCheck(hc, base); err != nil {
		return err
	}

	// 3) 管理端配置。
	admin := newGWClient(base)
	if err := admin.login(adminUser, adminPass); err != nil {
		return fmt.Errorf("登录失败（-admin-password 默认 admin12345）：%w", err)
	}
	adminID, mustChange, err := admin.me()
	if err != nil {
		return err
	}
	if mustChange {
		return fmt.Errorf("管理员仍处于「强制改密」状态，管理端接口会返回 40302；请先改密或换用其他管理员账号")
	}
	fmt.Printf("管理员已登录，userID=%s\n", adminID)

	// 确保钱包余额 > 0（预扣费要求余额非零）。
	if err := admin.recharge(adminID, 1_000_000, adminPass); err != nil {
		fmt.Printf("  [warn] 管理员充值未成功（可能密码不符）: %v\n", err)
	} else {
		fmt.Printf("管理员钱包已充值\n")
	}

	// 三个对外模型（每个仅绑定一条渠道 → 路由确定）。
	modelNames := map[string]string{
		protoChat: "demo-chat",
		protoResp: "demo-resp",
		protoAnth: "demo-anth",
	}
	externalModelID := map[string]string{}
	for _, proto := range []string{protoChat, protoResp, protoAnth} {
		id, err := admin.ensureModel(modelNames[proto])
		if err != nil {
			return fmt.Errorf("配置对外模型 %s: %w", modelNames[proto], err)
		}
		externalModelID[proto] = id
		fmt.Printf("对外模型 %s = %s\n", modelNames[proto], id)
	}

	// 三条渠道 + 密钥 + 内部模型。
	for _, proto := range []string{protoChat, protoResp, protoAnth} {
		chName := "ch-" + proto
		baseURL := mockBaseURL(proto)
		chID, err := admin.ensureChannel(chName, proto, baseURL)
		if err != nil {
			return fmt.Errorf("配置渠道 %s: %w", chName, err)
		}
		if _, err := admin.ensureKey(chID); err != nil {
			return fmt.Errorf("配置渠道 %s 密钥: %w", chName, err)
		}
		if _, err := admin.ensureChannelModel(chID, upstreamModelID, externalModelID[proto]); err != nil {
			return fmt.Errorf("配置渠道 %s 内部模型: %w", chName, err)
		}
		fmt.Printf("渠道 %s (%s @ %s) 就绪\n", chName, proto, baseURL)
	}

	// 网关调用令牌。
	token, err := admin.ensureToken(adminID)
	if err != nil {
		return fmt.Errorf("创建调用令牌: %w", err)
	}
	fmt.Printf("调用令牌已就绪\n\n")

	// 4) 跑矩阵。
	env := &matrixEnv{
		base:       strings.TrimRight(base, "/"),
		token:      token,
		admin:      admin,
		client:     hc,
		mocks:      mocks,
		externalID: modelNames,
	}
	rows := runMatrix(env)

	// 5) 打印结果表。
	printTable(rows)

	failed := 0
	for _, r := range rows {
		if !r.ok {
			failed++
		}
	}
	fmt.Printf("\n合计 %d 个用例，通过 %d，失败 %d\n", len(rows), len(rows)-failed, failed)
	if failed > 0 {
		printFailures(rows)
		return fmt.Errorf("%d 个用例失败", failed)
	}
	fmt.Println("全部通过 ✅")
	return nil
}

func mockBaseURL(proto string) string {
	switch proto {
	case protoChat:
		return "http://" + mockChatAddr
	case protoResp:
		return "http://" + mockRespAddr
	default:
		return "http://" + mockAnthAddr
	}
}

func healthCheck(hc *http.Client, base string) error {
	resp, err := hc.Get(strings.TrimRight(base, "/") + "/healthz")
	if err != nil {
		return fmt.Errorf("网关不可达 %s：%w", base, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("网关 /healthz 返回 %d", resp.StatusCode)
	}
	return nil
}

func printTable(rows []testRow) {
	pw, cw := len("path"), len("case")
	for _, r := range rows {
		if len(r.path) > pw {
			pw = len(r.path)
		}
		if len(r.caseName) > cw {
			cw = len(r.caseName)
		}
	}
	fmt.Printf("%-*s | %-*s | %-4s | %s\n", pw, "path", cw, "case", "rslt", "note")
	fmt.Println(strings.Repeat("-", pw+cw+40))
	for _, r := range rows {
		st := "PASS"
		if !r.ok {
			st = "FAIL"
		}
		note := r.note
		if r.ok {
			note = ""
		}
		fmt.Printf("%-*s | %-*s | %-4s | %s\n", pw, r.path, cw, r.caseName, st, note)
	}
}

func printFailures(rows []testRow) {
	type grp struct {
		path  string
		names []string
	}
	byPath := map[string][]string{}
	var order []string
	for _, r := range rows {
		if r.ok {
			continue
		}
		if _, ok := byPath[r.path]; !ok {
			order = append(order, r.path)
		}
		byPath[r.path] = append(byPath[r.path], r.caseName)
	}
	sort.Strings(order)
	fmt.Println("\n失败汇总:")
	for _, p := range order {
		fmt.Printf("  - %s: %s\n", p, strings.Join(byPath[p], ", "))
	}
	// 打印第一条失败详情，便于定位。
	for _, r := range rows {
		if !r.ok {
			fmt.Printf("\n首个失败详情 [%s | %s]:\n%s\n", r.path, r.caseName, r.note)
			break
		}
	}
}