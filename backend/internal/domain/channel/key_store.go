package channel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// ChannelKey 渠道密钥（运行时实体；配置与限流模板继承自渠道）。
// CredentialEnc 为库中 SM4 密文，对外脱敏；内存运行时保存解密明文。
type ChannelKey struct {
	ID            int64
	ChannelID     int64
	Name          string
	CredentialEnc string
	State         State
	LastErr       string
	DeletedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ChannelKeyEvent 对应 channel_key_events 表一行，记录一次密钥状态流转。
type ChannelKeyEvent struct {
	ID           int64
	ChannelKeyID int64
	FromState    State
	ToState      State
	Reason       string
	CreatedAt    time.Time
}

// channelKeyCols 列出 channel_keys 表查询时使用的全部列，与 DDL 列顺序一致。
const channelKeyCols = `id, channel_id, name, credential_enc, state, last_err, deleted_at, created_at, updated_at`

const channelKeyEventCols = `id, channel_key_id, from_state, to_state, reason, created_at`

// scanChannelKey 将一行扫描到 *ChannelKey。
func scanChannelKey(row interface{ Scan(...any) error }) (*ChannelKey, error) {
	var k ChannelKey
	var state string
	err := row.Scan(&k.ID, &k.ChannelID, &k.Name, &k.CredentialEnc, &state,
		&k.LastErr, &k.DeletedAt, &k.CreatedAt, &k.UpdatedAt)
	if err != nil {
		return nil, err
	}
	k.State = State(state)
	return &k, nil
}

func scanChannelKeyEvent(row interface{ Scan(...any) error }) (*ChannelKeyEvent, error) {
	var e ChannelKeyEvent
	var from, to string
	err := row.Scan(&e.ID, &e.ChannelKeyID, &from, &to, &e.Reason, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	e.FromState = State(from)
	e.ToState = State(to)
	return &e, nil
}

// KeyStore 提供 channel_keys / channel_key_events 表的基础数据访问。
type KeyStore struct {
	db *sql.DB
}

// NewKeyStore 创建密钥存储，db 为 pgx stdlib 连接池。
func NewKeyStore(db *sql.DB) *KeyStore {
	return &KeyStore{db: db}
}

// Insert 插入新密钥并返回回填主键 ID。同渠道同名冲突返回 ErrNameExists。
func (s *KeyStore) Insert(ctx context.Context, k ChannelKey) (int64, error) {
	if k.ID == 0 {
		k.ID = idgen.New()
	}
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO channel_keys(id, channel_id, name, credential_enc) VALUES($1,$2,$3,$4) RETURNING id`,
		k.ID, k.ChannelID, k.Name, k.CredentialEnc).Scan(&k.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return 0, ErrNameExists
		}
		return 0, err
	}
	return k.ID, nil
}

// List 查询某渠道下未删除密钥，按 id 升序。
func (s *KeyStore) List(ctx context.Context, channelID int64) ([]ChannelKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+channelKeyCols+` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`, channelID)
	if err != nil {
		return nil, fmt.Errorf("list channel keys: %w", err)
	}
	defer rows.Close()

	keys := make([]ChannelKey, 0, 8)
	for rows.Next() {
		k, err := scanChannelKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel key: %w", err)
		}
		keys = append(keys, *k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// ListAll 查询全部未删除密钥，按 channel_id, id 排序。
func (s *KeyStore) ListAll(ctx context.Context) ([]ChannelKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+channelKeyCols+` FROM channel_keys WHERE deleted_at IS NULL ORDER BY channel_id, id`)
	if err != nil {
		return nil, fmt.Errorf("list all channel keys: %w", err)
	}
	defer rows.Close()

	keys := make([]ChannelKey, 0, 8)
	for rows.Next() {
		k, err := scanChannelKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel key: %w", err)
		}
		keys = append(keys, *k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// GetByID 按主键查询未删除密钥；不存在返回 sql.ErrNoRows。
func (s *KeyStore) GetByID(ctx context.Context, id int64) (*ChannelKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+channelKeyCols+` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`, id)
	return scanChannelKey(row)
}

// GetByChannelName 按渠道+名称查询未删除密钥；不存在返回 sql.ErrNoRows。
func (s *KeyStore) GetByChannelName(ctx context.Context, channelID int64, name string) (*ChannelKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+channelKeyCols+` FROM channel_keys WHERE channel_id=$1 AND name=$2 AND deleted_at IS NULL`,
		channelID, name)
	return scanChannelKey(row)
}

// UpdateInfo 更新密钥名称与凭据。updateCred=false 时不动 credential_enc。
// 名称冲突返回 ErrNameExists；无匹配行返回 sql.ErrNoRows。
func (s *KeyStore) UpdateInfo(ctx context.Context, id int64, name, credentialEnc string, updateCred bool) error {
	var (
		res sql.Result
		err error
	)
	if updateCred {
		res, err = s.db.ExecContext(ctx,
			`UPDATE channel_keys SET name=$1, credential_enc=$2, updated_at=now() WHERE id=$3`,
			name, credentialEnc, id)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE channel_keys SET name=$1, updated_at=now() WHERE id=$2`, name, id)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrNameExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetState 仅更新密钥权威状态与最近错误，并刷新 updated_at，供状态机持久化使用。
func (s *KeyStore) SetState(ctx context.Context, id int64, state State, lastErr string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channel_keys SET state=$1, last_err=$2, updated_at=now() WHERE id=$3`,
		string(state), lastErr, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SoftDelete 软删除密钥并刷新 updated_at。
func (s *KeyStore) SoftDelete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE channel_keys SET deleted_at=now() WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// InsertEvent 记录一条密钥状态流转事件。
func (s *KeyStore) InsertEvent(ctx context.Context, keyID int64, from, to State, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channel_key_events(id, channel_key_id, from_state, to_state, reason) VALUES($1,$2,$3,$4,$5)`,
		idgen.New(), keyID, string(from), string(to), reason)
	return err
}

// ListEvents 查询密钥状态流转事件（created_at 倒序）；limit<=0 时取 50 条。
func (s *KeyStore) ListEvents(ctx context.Context, keyID int64, limit int) ([]ChannelKeyEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+channelKeyEventCols+` FROM channel_key_events WHERE channel_key_id=$1 ORDER BY created_at DESC LIMIT $2`,
		keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("list channel key events: %w", err)
	}
	defer rows.Close()

	events := make([]ChannelKeyEvent, 0, 8)
	for rows.Next() {
		e, err := scanChannelKeyEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel key event: %w", err)
		}
		events = append(events, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
