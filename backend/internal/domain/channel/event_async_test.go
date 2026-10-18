package channel

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

type keyEventRecord struct {
	evID, keyID int64
	from, to    State
	reason      string
}

type modelEventRecord struct {
	evID, channelID int64
	modelID         string
	from, to        State
	reason          string
}

// fakeEventSink 记录经 sink 提交的事件（测试断言「事件走 sink」）。
type fakeEventSink struct {
	keyEvents   []keyEventRecord
	modelEvents []modelEventRecord
}

func (f *fakeEventSink) SubmitKeyEvent(_ context.Context, evID, keyID int64, from, to State, reason string) error {
	f.keyEvents = append(f.keyEvents, keyEventRecord{evID: evID, keyID: keyID, from: from, to: to, reason: reason})
	return nil
}

func (f *fakeEventSink) SubmitModelEvent(_ context.Context, evID, channelID int64, modelID string, from, to State, reason string) error {
	f.modelEvents = append(f.modelEvents, modelEventRecord{evID: evID, channelID: channelID, modelID: modelID, from: from, to: to, reason: reason})
	return nil
}

// newManagerForEventTest 构造仅用于 persist* 直调的 Manager（sqlmock 底座）。
func newManagerForEventTest(t *testing.T) (*Manager, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mgr := NewManager(NewStore(db), NewChannelModelStore(db), testSM4Key, nil, nil, nil, NewKeyStore(db))
	return mgr, mock
}

// TestEventAsync_KeyTransition 注入 fake sink 后：密钥事件经 sink 提交，
// 而 channel_keys.state 仍同步落库（sqlmock 断言 UPDATE 已执行、无同步 INSERT 事件）。
func TestEventAsync_KeyTransition(t *testing.T) {
	mgr, mock := newManagerForEventTest(t)
	sink := &fakeEventSink{}
	mgr.SetEventSink(sink)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET state=$1, last_err=$2, updated_at=now() WHERE id=$3`)).
		WithArgs(string(StateDisabled), "", int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mgr.persistKeyTransition(11, StateNormal, StateDisabled, "channel disabled: usage missing")

	if len(sink.keyEvents) != 1 {
		t.Fatalf("密钥事件应经 sink 提交一次，got %d", len(sink.keyEvents))
	}
	ev := sink.keyEvents[0]
	if ev.evID == 0 {
		t.Fatalf("事件 ID 应由调用方预生成（非零）")
	}
	if ev.keyID != 11 || ev.from != StateNormal || ev.to != StateDisabled || ev.reason != "channel disabled: usage missing" {
		t.Fatalf("sink 事件参数异常：%+v", ev)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("状态列应同步更新（UPDATE 立即执行）：%v", err)
	}
}

// TestEventAsync_ModelTransition 注入 fake sink 后：模型事件经 sink 提交，
// 而 channel_models.state 仍同步落库。
func TestEventAsync_ModelTransition(t *testing.T) {
	mgr, mock := newManagerForEventTest(t)
	sink := &fakeEventSink{}
	mgr.SetEventSink(sink)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET state=$1, updated_at=now() WHERE channel_id=$2 AND internal_model_id=$3`)).
		WithArgs(string(StateDrain), int64(5), "gpt-4o").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mgr.persistModelTransition(5, "gpt-4o", StateNormal, StateDrain, "probe fail")

	if len(sink.modelEvents) != 1 {
		t.Fatalf("模型事件应经 sink 提交一次，got %d", len(sink.modelEvents))
	}
	ev := sink.modelEvents[0]
	if ev.evID == 0 {
		t.Fatalf("事件 ID 应由调用方预生成（非零）")
	}
	if ev.channelID != 5 || ev.modelID != "gpt-4o" || ev.from != StateNormal || ev.to != StateDrain || ev.reason != "probe fail" {
		t.Fatalf("sink 事件参数异常：%+v", ev)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("状态列应同步更新（UPDATE 立即执行）：%v", err)
	}
}

// TestEventAsync_NilSinkFallsBackSync 未注入 sink 时退回同步 InsertEvent + 同步 SetState（保持既有行为）。
func TestEventAsync_NilSinkFallsBackSync(t *testing.T) {
	mgr, mock := newManagerForEventTest(t)

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO channel_key_events(id, channel_key_id, from_state, to_state, reason) VALUES($1,$2,$3,$4,$5)`)).
		WithArgs(sqlmock.AnyArg(), int64(12), string(StateNormal), string(StateDrain), "manual drain").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET state=$1, last_err=$2, updated_at=now() WHERE id=$3`)).
		WithArgs(string(StateDrain), "", int64(12)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mgr.persistKeyTransition(12, StateNormal, StateDrain, "manual drain")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未注入 sink 时应同步更新状态并同步插入事件：%v", err)
	}
}

// TestHandleReplayErrPermanent 锁定 P2-10：外键冲突 / ErrNoRows 属不可恢复重放，应丢弃（返回 nil）
// 并计数（否则 WAL 记录会永久重试）；可恢复错误必须原样返回以便保留 WAL 重试。
func TestHandleReplayErrPermanent(t *testing.T) {
	before := droppedEventReplays()
	// 不可恢复：外键冲突（引用渠道/密钥被硬删除）。
	if err := handleReplayErr("channel_key_event", "k", &pgconn.PgError{Code: "23503"}); err != nil {
		t.Fatalf("外键冲突应被丢弃（返回 nil），got %v", err)
	}
	// 不可恢复：目标行缺失。
	if err := handleReplayErr("channel_key_event", "k", sql.ErrNoRows); err != nil {
		t.Fatalf("ErrNoRows 应被丢弃（返回 nil），got %v", err)
	}
	if got := droppedEventReplays() - before; got != 2 {
		t.Fatalf("不可恢复重放应计数 2，got %d", got)
	}
	// 可恢复：瞬时 DB 故障原样返回。
	sentinel := errors.New("transient db error")
	if err := handleReplayErr("channel_key_event", "k", sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("可恢复错误应原样返回，got %v", err)
	}
	// nil 透传。
	if err := handleReplayErr("channel_key_event", "k", nil); err != nil {
		t.Fatalf("nil 应返回 nil，got %v", err)
	}
}
