package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/team/llmgateway/internal/pkg/asyncwrite"
)

// keyEventPayload 密钥状态事件的持久负载，供崩溃重放重建事件行（含预生成事件 ID）。
type keyEventPayload struct {
	EventID int64  `json:"event_id"`
	KeyID   int64  `json:"key_id"`
	From    State  `json:"from"`
	To      State  `json:"to"`
	Reason  string `json:"reason"`
}

// modelEventPayload 模型状态事件的持久负载，供崩溃重放重建事件行（含预生成事件 ID）。
type modelEventPayload struct {
	EventID   int64  `json:"event_id"`
	ChannelID int64  `json:"channel_id"`
	ModelID   string `json:"model_id"`
	From      State  `json:"from"`
	To        State  `json:"to"`
	Reason    string `json:"reason"`
}

// asyncEventSink 是 EventSink 的 asyncwrite 实现：渠道状态事件 INSERT 异步落库。
// 状态列（channel_keys.state / channel_models.state）不在此处，仍由 Manager 同步落库。
//
// 幂等与保序：事件 ID（雪花）预生成并作为 IdempotencyKey，同时底层 INSERT 用
// ON CONFLICT (id) DO NOTHING，双重保证重复提交/崩溃重放不产生重复事件；
// Key 取密钥 ID / 渠道+模型，使同一实体的事件同分片保序。
type asyncEventSink struct {
	w        *asyncwrite.Writer
	keyStore *KeyStore
	cmStore  *channelModelStore
}

// 编译期断言：asyncEventSink 满足 EventSink。
var _ EventSink = (*asyncEventSink)(nil)

// NewAsyncEventSink 创建渠道状态事件异步 sink，并注册两类事件的重放函数。
// w 为 nil 时各提交方法同步直调底层 store（安全兜底）。
func NewAsyncEventSink(w *asyncwrite.Writer, keyStore *KeyStore, cmStore *channelModelStore) EventSink {
	s := &asyncEventSink{w: w, keyStore: keyStore, cmStore: cmStore}
	if w != nil {
		w.SetReplayFunc(asyncwrite.KindChannelKeyEvent, s.replayKeyEvent)
		w.SetReplayFunc(asyncwrite.KindChannelModelEvent, s.replayModelEvent)
	}
	return s
}

// SubmitKeyEvent 提交一条密钥状态事件（异步）。
func (s *asyncEventSink) SubmitKeyEvent(ctx context.Context, evID, keyID int64, from, to State, reason string) error {
	if s.keyStore == nil {
		return nil
	}
	if s.w == nil {
		return s.keyStore.InsertEventWithID(ctx, evID, keyID, from, to, reason)
	}
	payload, err := json.Marshal(keyEventPayload{EventID: evID, KeyID: keyID, From: from, To: to, Reason: reason})
	if err != nil {
		return err
	}
	return s.w.Submit(asyncwrite.Job{
		Kind:           asyncwrite.KindChannelKeyEvent,
		Key:            strconv.FormatInt(keyID, 10),
		IdempotencyKey: strconv.FormatInt(evID, 10),
		Payload:        payload,
		Exec: func(c context.Context) error {
			return s.keyStore.InsertEventWithID(c, evID, keyID, from, to, reason)
		},
	})
}

// SubmitModelEvent 提交一条模型状态事件（异步）。
func (s *asyncEventSink) SubmitModelEvent(ctx context.Context, evID, channelID int64, modelID string, from, to State, reason string) error {
	if s.cmStore == nil {
		return nil
	}
	if s.w == nil {
		return s.cmStore.InsertModelEventWithID(ctx, evID, channelID, modelID, from, to, reason)
	}
	payload, err := json.Marshal(modelEventPayload{EventID: evID, ChannelID: channelID, ModelID: modelID, From: from, To: to, Reason: reason})
	if err != nil {
		return err
	}
	return s.w.Submit(asyncwrite.Job{
		Kind:           asyncwrite.KindChannelModelEvent,
		Key:            strconv.FormatInt(channelID, 10) + "|" + modelID,
		IdempotencyKey: strconv.FormatInt(evID, 10),
		Payload:        payload,
		Exec: func(c context.Context) error {
			return s.cmStore.InsertModelEventWithID(c, evID, channelID, modelID, from, to, reason)
		},
	})
}

// replayKeyEvent 崩溃恢复重放：解码负载并以预生成 ID 幂等写入密钥事件。
func (s *asyncEventSink) replayKeyEvent(ctx context.Context, key string, payload []byte) error {
	if s.keyStore == nil {
		return nil
	}
	var pl keyEventPayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		return fmt.Errorf("decode channel key event payload: %w", err)
	}
	return s.keyStore.InsertEventWithID(ctx, pl.EventID, pl.KeyID, pl.From, pl.To, pl.Reason)
}

// replayModelEvent 崩溃恢复重放：解码负载并以预生成 ID 幂等写入模型事件。
func (s *asyncEventSink) replayModelEvent(ctx context.Context, key string, payload []byte) error {
	if s.cmStore == nil {
		return nil
	}
	var pl modelEventPayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		return fmt.Errorf("decode channel model event payload: %w", err)
	}
	return s.cmStore.InsertModelEventWithID(ctx, pl.EventID, pl.ChannelID, pl.ModelID, pl.From, pl.To, pl.Reason)
}
