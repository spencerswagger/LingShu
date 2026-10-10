package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
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
	Sessions() *router.SessionRegistry
}

// ChannelResolver 双运行时解析与结果回喂抽象（channel.Manager 满足）。
// 密钥为运行时实体：真实调用同时回喂密钥级与内部模型级状态机（密钥+模型双回喂）。
type ChannelResolver interface {
	GetRuntime(id int64) (*channel.RuntimeChannel, bool)
	GetKeyRuntime(keyID int64) (*channel.KeyRuntime, bool)
	GetModelRuntime(channelID, modelRowID int64) (*channel.ModelRuntime, bool)
	FeedResult(keyID int64, modelRowID int64, f channel.Feedback)
	KeyMaxSessions(keyID int64) int
	SessionTTL(channelID int64) int
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

// TagKVResolver 由令牌 TagID 解析其 KV 标签集合（装配层用 tag.Service 实现）。
// tagID 为 nil 时返回 nil map。
type TagKVResolver func(ctx context.Context, tagID *int64) (map[string]string, error)

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
	tagKV           TagKVResolver
	pricing         PricingModeResolver
	balance         BalanceResolver
	credit          CreditOps
	providerFactory ProviderFactory
	touchLastUsed   func(ctx context.Context, tokenID int64) error
	listEnabled     func(ctx context.Context) ([]model.ExternalModel, error)
	logger          *slog.Logger
	now             func() time.Time
	client          *http.Client // 非流式上游客户端（默认 300s 超时）
	streamClient    *http.Client // 流式上游客户端（不设 body 超时，由 ctx 控制）
}

// GatewayConfig 是 NewGateway 的入参。
type GatewayConfig struct {
	Tokens        TokenLookup
	Router        Router
	Channels      ChannelResolver
	Billing       BillingRecorder
	Models        ModelResolver
	ChannelModels ChannelModelResolver
	TagKV         TagKVResolver
	Pricing       PricingModeResolver
	// Balance 可选：余额前置检查（余额 <=0 时拒绝发起上游调用）。nil 时不检查（兼容旧装配）。
	Balance BalanceResolver
	// Credit 可选：三阶段计费（预扣/退款）。装配后启用「预扣-结算-退补」；nil 时退化为 balance 检查。
	Credit          CreditOps
	ProviderFactory ProviderFactory
	Logger          *slog.Logger
	Now             func() time.Time
	Client          *http.Client // nil 时默认 300s 超时
	StreamClient    *http.Client // nil 时派生自 Client，但 body 不超时
	// TouchLastUsed 令牌鉴权通过后异步记录使用时间（可为空，热路径不阻塞）。
	TouchLastUsed func(ctx context.Context, tokenID int64) error
	// ListEnabledModels 返回启用中的对外模型（GET /v1/models 用；为空时该端点返回空列表）。
	ListEnabledModels func(ctx context.Context) ([]model.ExternalModel, error)
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
	return &Gateway{
		tokens:          cfg.Tokens,
		router:          cfg.Router,
		channels:        cfg.Channels,
		billing:         cfg.Billing,
		models:          cfg.Models,
		channelModels:   cfg.ChannelModels,
		tagKV:           cfg.TagKV,
		pricing:         cfg.Pricing,
		balance:         cfg.Balance,
		credit:          cfg.Credit,
		providerFactory: cfg.ProviderFactory,
		touchLastUsed:   cfg.TouchLastUsed,
		listEnabled:     cfg.ListEnabledModels,
		logger:          cfg.Logger,
		now:             cfg.Now,
		client:          cfg.Client,
		streamClient:    cfg.StreamClient,
	}
}

// SetCallLogs 注入调用日志存储（nil=不采集调用日志；测试与未装配场景安全）。
func (g *Gateway) SetCallLogs(s CallLogStore) { g.callLogs = s }

// DefaultProviderFactory 默认协议工厂：目前仅 openai-compat。
func DefaultProviderFactory(protocol, endpoint string) (Provider, bool) {
	if protocol != channel.ProtocolOpenAICompat {
		return nil, false
	}
	return NewOpenAIProvider(endpoint), true
}

// Handler 返回挂载了 /v1 各端点的 http.Handler。
//
//   - GET  /v1/models
//   - POST /v1/chat/completions
//   - POST /v1/completions
//   - POST /v1/embeddings
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", g.serveWith(EndpointChatCompletions))
	mux.HandleFunc("POST /v1/completions", g.serveWith(EndpointCompletions))
	mux.HandleFunc("POST /v1/embeddings", g.serveWith(EndpointEmbeddings))
	return mux
}

func (g *Gateway) serveWith(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.serve(w, r, endpoint)
	}
}
