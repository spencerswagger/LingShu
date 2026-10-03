package session

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestRegistry_CheckOK(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(func(ctx context.Context, userID int64) (Entry, error) {
		return Entry{Version: 3, Status: "ACTIVE", MustChange: false}, nil
	})
	if mc, err := r.Check(ctx, 1, 3); err != nil || mc {
		t.Fatalf("expected ok, got mc=%v err=%v", mc, err)
	}
	// 二次调用命中缓存
	if mc, err := r.Check(ctx, 1, 3); err != nil || mc {
		t.Fatalf("expected cached ok, got mc=%v err=%v", mc, err)
	}
}

func TestRegistry_CheckVersionMismatch(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(func(ctx context.Context, userID int64) (Entry, error) {
		return Entry{Version: 3, Status: "ACTIVE", MustChange: false}, nil
	})
	if _, err := r.Check(ctx, 1, 2); err == nil {
		t.Fatal("expected version mismatch error")
	}
}

func TestRegistry_CheckDisabled(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(func(ctx context.Context, userID int64) (Entry, error) {
		return Entry{Version: 1, Status: "DISABLED", MustChange: false}, nil
	})
	if _, err := r.Check(ctx, 9, 1); err == nil {
		t.Fatal("expected disabled error")
	}
}

func TestRegistry_CheckMissingUser(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(func(ctx context.Context, userID int64) (Entry, error) {
		return Entry{}, sql.ErrNoRows
	})
	if _, err := r.Check(ctx, 404, 1); err == nil {
		t.Fatal("expected not found -> revoked error")
	}
}

func TestRegistry_SetThenCheck(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(func(ctx context.Context, userID int64) (Entry, error) {
		return Entry{}, errors.New("should not load")
	})
	r.Set(5, Entry{Version: 8, Status: "ACTIVE", MustChange: true})
	mc, err := r.Check(ctx, 5, 8)
	if err != nil || !mc {
		t.Fatalf("expected mustChange from cache, got mc=%v err=%v", mc, err)
	}
}
