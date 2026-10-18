package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/team/llmgateway/internal/domain/automodel"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/router"
)

// TokenLookup 令牌校验抽象（identity.TokenService 满足），便于测试注入 fake。
type TokenLookup interface {
	LookupByPlain(ctx context.Context, plain string) (*identity.Token, error)
}

// Router 路由选择抽象（router.Engine 满足）。
type Router interface {
	Route(ctx context.Context, spec router.RouteSpec) (*router.RouteResult, error)
	RouteAll(ctx context.Context, spec router.RouteSpec) ([]*router.ChannelRoute, error)
	// RouteAllWithExcluded 与 RouteAll 同语义，额外返回路由层被排除的候选
	// （决策轨迹用；路由失败时也返回已收集的排除项）。
	RouteAllWithExcluded(ctx context.Context, spec router.RouteSpec) (*router.RouteAllResult, error)
	Sessions() *router.SessionRegistry
}

// ChannelResolver 双运行时解析与结果回喂抽象（channel.Manager 满足）。
// 密钥为运行时实体：真实调用同时回喂密钥级与内部模型级状态机（密钥+模型双回喂）。
type ChannelResolver interface {
	GetRuntime(id int64) (*channel.RuntimeChannel, bool)
	GetKeyRuntime(keyID int64) (*channel.KeyRuntime, bool)
	GetModelRuntime(channelID, modelRowID int64) (*channel.ModelRuntime, bool)
	FeedResult(keyID int64, modelRowID int64, f channel.Feedback)
	// FeedResultGated 回喂结果并带「是否计入健康」门控：countIntoHealth=false 完全不影响熔断。
	FeedResultGated(keyID int64, modelRowID int64, f channel.Feedback, countIntoHealth bool)
	KeyMaxSessions(keyID int64) int
	// ForceKeyState 手动切换密钥状态并持久化（内存状态机 + channel_keys.state 落库 +
	// channel_key_events 事件，与 KeyService.ForceState 同源）；供协议级错误策略按动作(drain|disable)处置。
	// 区别于仅内存的 ManualSetKeyState：策略触发的密钥处置必须落库，重启后仍生效（与模型处置对称）。
	ForceKeyState(ctx context.Context, keyID int64, action string) error
	// ManualSetModelState 手动设置内部模型状态；供协议级错误策略按动作(drain|disable)处置。
	ManualSetModelState(channelID, modelRowID int64, st channel.State) error
	// DisableChannel 将渠道及其全部密钥置为禁用（AC-17：上游 usage 缺失/无法解析时）。
	DisableChannel(channelID int64)
}

// SessionExpiryResolver 会话过期 TTL 计算抽象（router.SessionExpiryProvider 满足）。
type SessionExpiryResolver interface {
	TTL(ctxTokens, turns int) time.Duration
}

// BillingRecorder 计费抽象（billing.Service 满足）。
type BillingRecorder interface {
	Record(ctx context.Context, req billing.RecordReq) (*billing.Record, error)
	// EstimateCredits 与 Record 同口径的预扣金额估算（不落库、不扣费）。
	EstimateCredits(ctx context.Context, req billing.RecordReq) (float64, error)
	// EstimateBreakdown 预扣估算 + 系数明细：与 EstimateCredits 同一口径，额外返回
	// 本次估算采用的时段/分档系数与 R，供预扣失败账单渲染与正常结算一致的多行公式。
	EstimateBreakdown(ctx context.Context, req billing.RecordReq) (credits, timeCoeff, ctxCoeff float64, r int64, err error)
}

// ModelResolver 对外模型查询抽象（model.Service 满足）。
type ModelResolver interface {
	GetByExternalName(ctx context.Context, externalName string) (*model.ExternalModel, error)
}

// ChannelModelResolver 渠道内部模型查询抽象（channel 包 channelModelStore 满足）。
// 计费按 cost 模式时需取选中渠道的成本定价与模型级时段/分档。
type ChannelModelResolver interface {
	GetByChannelAndInternal(ctx context.Context, channelID int64, internalModelID string) (*channel.ChannelModel, error)
}

// ProtocolResolver 渠道协议查询抽象（channelprotocol.Store 满足）。
// 网关据此读取协议级错误策略（error_policy）等协议配置；nil=未装配（错误策略不生效，默认安全态）。
type ProtocolResolver interface {
	GetByID(ctx context.Context, id int64) (*channelprotocol.ChannelProtocol, error)
}

// ProtocolErrorStatsRecorder 协议级错误统计写入抽象（channelprotocol.Store 满足）。
// 网关在命中错误策略时按错误类别累加当日计数；nil=未装配（不记统计，行为不变）。
type ProtocolErrorStatsRecorder interface {
	IncrProtocolErrorStat(ctx context.Context, protocolID int64, category string, day time.Time) error
}

// TagKVResolver 由令牌 TagID 解析其 KV 标签集合（装配层用 tag.Service 实现）。
// tagID 为 nil 时返回 nil map。
type TagKVResolver func(ctx context.Context, tagID *int64) (map[string]string, error)

// AutoResolver 解析「请求模型名是否为 Auto 模型」并返回其有序成员
// （automodel.Service 满足）。返回 (成员列表, 是否 Auto, err)：
// 未命中返回 (nil,false,nil)，交由上层按普通对外模型处理；停用的 Auto 亦视为未命中。
type AutoResolver interface {
	ResolveMembers(ctx context.Context, name string, ownerUserID *int64) ([]automodel.Member, bool, error)
}

// PricingModeResolver 由用户 ID 解析计费模式（"sale"/"cost"，装配层查 users 表实现）。
type PricingModeResolver func(ctx context.Context, userID int64) (string, error)

// BalanceResolver 查询用户积分余额（网关余额前置检查用；查询失败由调用方决定放行/拦截）。
type BalanceResolver func(ctx context.Context, userID int64) (float64, error)

// CreditOps 三阶段计费能力（对齐 new-api pre-consume/settle/refund）：
// PreConsume 原子预扣（余额不足返回 40201，流水直接关联会话）；
// Settle 在预扣基础上按差额多退少补；Balance 读余额；
// Refund 全额退回预扣（全部候选失败时）。
type CreditOps interface {
	Balance(ctx context.Context, userID int64) (float64, error)
	PreConsume(ctx context.Context, userID int64, amount float64, billingID, sessionID, remark string) error
	Settle(ctx context.Context, userID int64, delta float64, sessionID, remark string) error
	Refund(ctx context.Context, userID int64, amount float64, remark string) error
}

// ProviderFactory 依据渠道协议 + 端点创建 Provider。
type ProviderFactory func(protocol, endpoint string) (Provider, bool)

// Gateway 是网关核心对象，串起认证、路由、渠道、限流、转发、计费。
//
// 依赖全部以接口/函数注入，便于测试用 fake 替换，无需启动 PG。
// LastUsedAt 更新不在此热路径落库（避免额外 IO），由任务 8 查询时/异步处理。
type Gateway struct {
	tokens          TokenLookup
	router          Router
	channels        ChannelResolver
	billing         BillingRecorder
	callLogs        CallLogStore // 调用日志写入（nil=不采集，单测安全）
	models          ModelResolver
	channelModels   ChannelModelResolver
	protocols       ProtocolResolver           // 渠道协议查询（读协议级错误策略）；nil=未装配
	protocolStats   ProtocolErrorStatsRecorder // 协议级错误统计写入；nil=未装配
	tagKV           TagKVResolver
	auto            AutoResolver // Auto 模型解析（nil=不启用 Auto；未命中时零行为差异）
	pricing         PricingModeResolver
	balance         BalanceResolver
	credit          CreditOps
	providerFactory ProviderFactory
	sessionExpiry   SessionExpiryResolver
	touchLastUsed   func(ctx context.Context, tokenID int64) error
	listEnabled     func(ctx context.Context) ([]model.ExternalModel, error)
	// listAutoModels 返回对某用户可见的启用 Auto（全局 + 该用户私有），用于 GET /v1/models。
	listAutoModels func(ctx context.Context, ownerUserID int64) ([]automodel.AutoModel, error)
	logger         *slog.Logger
	now            func() time.Time
	client         *http.Client // 非流式上游客户端（默认 300s 超时）
	streamClient   *http.Client // 流式上游客户端（不设 body 超时，由 ctx 控制）
	// upstreamRespHeaders 全局上游响应头白名单（默认空，未配置时返回 nil）。
	upstreamRespHeaders func() []string
}

// GatewayConfig 是 NewGateway 的入参。
type GatewayConfig struct {
	Tokens        TokenLookup
	Router        Router
	Channels      ChannelResolver
	Billing       BillingRecorder
	Models        ModelResolver
	ChannelModels ChannelModelResolver
	// Protocols 可选：渠道协议查询（读协议级错误策略）。nil=未装配（错误策略不生效）。
	Protocols ProtocolResolver
	// ProtocolErrorStats 可选：协议级错误统计写入（命中策略时累加）。nil=未装配（不记统计）。
	ProtocolErrorStats ProtocolErrorStatsRecorder
	TagKV              TagKVResolver
	// Auto 可选：Auto 模型解析（nil 时不启用 Auto，请求名一律按普通对外模型处理）。
	Auto    AutoResolver
	Pricing PricingModeResolver
	// Balance 可选：余额前置检查（余额 <=0 时拒绝发起上游调用）。nil 时不检查（兼容旧装配）。
	Balance BalanceResolver
	// Credit 可选：三阶段计费（预扣/退款）。装配后启用「预扣-结算-退补」；nil 时退化为 balance 检查。
	Credit          CreditOps
	ProviderFactory ProviderFactory
	// SessionExpiry 可选：会话过期 TTL 计算（上下文长度 + 用户对话次数双维度分档）。
	// nil 时 handler 使用内置兜底时长。
	SessionExpiry SessionExpiryResolver
	Logger        *slog.Logger
	Now           func() time.Time
	Client        *http.Client // nil 时默认 300s 超时
	StreamClient  *http.Client // nil 时派生自 Client，但 body 不超时
	// TouchLastUsed 令牌鉴权通过后异步记录使用时间（可为空，热路径不阻塞）。
	TouchLastUsed func(ctx context.Context, tokenID int64) error
	// ListEnabledModels 返回启用中的对外模型（GET /v1/models 用；为空时该端点返回空列表）。
	ListEnabledModels func(ctx context.Context) ([]model.ExternalModel, error)
	// ListAutoModels 可选：返回对某用户可见的启用 Auto（全局 + 该用户私有），
	// 用于 GET /v1/models 一并列出。nil 时不列 Auto。
	ListAutoModels func(ctx context.Context, ownerUserID int64) ([]automodel.AutoModel, error)
	// UpstreamResponseHeaderWhitelist 读取全局上游响应头白名单（sys_configs，默认空）。
	// 未配置/读取失败按空处理（不透传任何上游头，维持渠道透明）。
	UpstreamResponseHeaderWhitelist func() ([]string, error)
}

// NewGateway 构造网关。缺省项使用合理默认值。
func NewGateway(cfg GatewayConfig) *Gateway {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ProviderFactory == nil {
		cfg.ProviderFactory = DefaultProviderFactory
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 300 * time.Second}
	}
	if cfg.StreamClient == nil {
		streamClient := *cfg.Client
		streamClient.Timeout = 0
		cfg.StreamClient = &streamClient
	}
	var respHeaders func() []string
	if cfg.UpstreamResponseHeaderWhitelist != nil {
		respHeaders = func() []string {
			if cfg.UpstreamResponseHeaderWhitelist == nil {
				return nil
			}
			names, err := cfg.UpstreamResponseHeaderWhitelist()
			if err != nil {
				return nil
			}
			return names
		}
	}
	return &Gateway{
		tokens:              cfg.Tokens,
		router:              cfg.Router,
		channels:            cfg.Channels,
		billing:             cfg.Billing,
		models:              cfg.Models,
		channelModels:       cfg.ChannelModels,
		protocols:           cfg.Protocols,
		protocolStats:       cfg.ProtocolErrorStats,
		tagKV:               cfg.TagKV,
		auto:                cfg.Auto,
		pricing:             cfg.Pricing,
		balance:             cfg.Balance,
		credit:              cfg.Credit,
		providerFactory:     cfg.ProviderFactory,
		sessionExpiry:       cfg.SessionExpiry,
		touchLastUsed:       cfg.TouchLastUsed,
		listEnabled:         cfg.ListEnabledModels,
		listAutoModels:      cfg.ListAutoModels,
		logger:              cfg.Logger,
		now:                 cfg.Now,
		client:              cfg.Client,
		streamClient:        cfg.StreamClient,
		upstreamRespHeaders: respHeaders,
	}
}

// SetCallLogs 注入调用日志存储（nil=不采集调用日志；测试与未装配场景安全）。
func (g *Gateway) SetCallLogs(s CallLogStore) { g.callLogs = s }

// upstreamHeaders 返回全局上游响应头白名单（未配置/读取失败返回 nil=不透传任何上游头）。
func (g *Gateway) upstreamHeaders() []string {
	if g.upstreamRespHeaders == nil {
		return nil
	}
	return g.upstreamRespHeaders()
}

// DefaultProviderFactory 依据基础协议 + 端点创建 Provider。
// openai-compat 为历史枚举值，按 openai-chat 语义解析。
func DefaultProviderFactory(protocol, endpoint string) (Provider, bool) {
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolOpenAIChat:
		return NewOpenAIProvider(endpoint), true
	case channelprotocol.ProtocolOpenAIResponses:
		return NewResponsesProvider(), true
	case channelprotocol.ProtocolAnthropicMessages:
		return NewAnthropicProvider(), true
	}
	return nil, false
}

// InboundPrefix 是网关对外端点的路径前缀：Handler 注册、server 挂载与异常日志的
// endpoint 字段共用同一常量，避免前缀字面量在多处重复、改挂载点时日志与访问日志失配。
const InboundPrefix = "/v1/"

// Handler 返回挂载了 /v1 各端点的 http.Handler。
//
//   - GET  /v1/models
//   - POST /v1/chat/completions  (openai-chat)
//   - POST /v1/completions       (openai-chat)
//   - POST /v1/embeddings        (openai-chat)
//   - POST /v1/responses         (openai-responses)
//   - POST /v1/messages          (anthropic-messages)
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+InboundPrefix+"models", g.handleModels)
	mux.HandleFunc("POST "+InboundPrefix+EndpointChatCompletions, g.serveWith(EndpointChatCompletions))
	mux.HandleFunc("POST "+InboundPrefix+EndpointCompletions, g.serveWith(EndpointCompletions))
	mux.HandleFunc("POST "+InboundPrefix+EndpointEmbeddings, g.serveWith(EndpointEmbeddings))
	mux.HandleFunc("POST "+InboundPrefix+EndpointResponses, g.serveWith(EndpointResponses))
	mux.HandleFunc("POST "+InboundPrefix+EndpointMessages, g.serveWith(EndpointMessages))
	return mux
}

func (g *Gateway) serveWith(endpoint string) http.HandlerFunc {
	proto := protocolForEndpoint(endpoint)
	return func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, endpoint, proto)
	}
}
