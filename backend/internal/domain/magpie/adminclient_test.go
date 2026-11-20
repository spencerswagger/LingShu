package magpie

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeMagpiePage 复刻页面鉴权：?k= 种 cookie；之后 /api/caller-keys* 需要 cookie。
// keysByID 复刻 magpie「ID 为稳定键、secret 全局唯一」的存储形状（rename/rotate 按 ID）。
func fakeMagpiePage(t *testing.T, key string) (*httptest.Server, *sync.Map) {
	store := &sync.Map{} // name -> secret
	mux := http.NewServeMux()
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		c, err := r.Cookie("magpie_web_3430")
		if err != nil || c.Value != key {
			http.Error(w, "no sign-in cookie", 401)
			return false
		}
		return true
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("k") != key {
			http.Error(w, "bad key", 401)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "magpie_web_3430", Value: key, Path: "/"})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("/api/caller-keys", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		rows := []map[string]any{}
		i := 0
		store.Range(func(k, v any) bool {
			i++
			rows = append(rows, map[string]any{"id": k.(string) + "-id", "name": k})
			return true
		})
		json.NewEncoder(w).Encode(map[string]any{"keys": rows})
	})
	mux.HandleFunc("/api/caller-keys/add-key", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if _, ok := store.Load(in["name"]); ok {
			http.Error(w, "exists", 500)
			return
		}
		// 复刻真机：secret 全局唯一（Another gateway key already has this key）。
		dup := false
		store.Range(func(_, v any) bool {
			if v.(string) == in["secret"] {
				dup = true
				return false
			}
			return true
		})
		if dup {
			http.Error(w, "magpie: Another gateway key already has this key", 400)
			return
		}
		store.Store(in["name"], in["secret"])
		json.NewEncoder(w).Encode(map[string]any{"secret": in["secret"]})
	})
	mux.HandleFunc("/api/caller-keys/rotate-key", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		store.Store(in["name"], in["secret"])
		json.NewEncoder(w).Encode(map[string]any{"secret": in["secret"]})
	})
	mux.HandleFunc("/api/caller-keys/remove-key", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		store.Delete(in["name"])
		w.Write([]byte("{}"))
	})
	// rename-key：真机 UI 同款端点；ID/secret 不变，只改名字。名字 >120 字符 400（真机校验）。
	mux.HandleFunc("/api/caller-keys/rename-key", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		id, _ := in["key"].(string)
		name, _ := in["name"].(string)
		var oldName string
		found := false
		store.Range(func(k, v any) bool {
			if k.(string)+"-id" == id {
				oldName, found = k.(string), true
				return false
			}
			return true
		})
		if !found {
			http.Error(w, "magpie: Key not found", 404)
			return
		}
		if len([]rune(name)) > 120 {
			http.Error(w, "magpie: Use a name between 1 and 120 characters", 400)
			return
		}
		secret, _ := store.Load(oldName)
		store.Delete(oldName)
		store.Store(name, secret)
		w.Write([]byte("{}"))
	})
	return httptest.NewServer(mux), store
}

func TestPageAdmin_EnsureAndDelete(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	srv, store := fakeMagpiePage(t, key)
	defer srv.Close()
	a := NewPageAdmin(srv.URL, key, nil)
	ctx := context.Background()

	sec1, err := a.EnsureCallerKey(ctx, "ls-token-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || sec1 == "" {
		t.Fatalf("ensure1: %v %q", err, sec1)
	}
	if v, _ := store.Load("ls-token-1"); v != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("stored=%v", v)
	}
	// 重入：名字已存在 → 走 rotate，仍是我们的 secret
	sec2, err := a.EnsureCallerKey(ctx, "ls-token-1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || sec2 != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("ensure2: %v %q", err, sec2)
	}
	if v, _ := store.Load("ls-token-1"); v != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("rotate stored=%v", v)
	}
	if err := a.DeleteCallerKey(ctx, "ls-token-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := store.Load("ls-token-1"); ok {
		t.Fatal("still exists")
	}
	// 幂等删除
	if err := a.DeleteCallerKey(ctx, "ls-token-1"); err != nil {
		t.Fatalf("delete idempotent: %v", err)
	}
}

// TestPageAdmin_RenameCallerKey 锁 rename-key 语义：ID/secret 不变、幂等、
// 目标名已存在视为成功、旧名不存在视为成功（回填竞态不卡死）。
func TestPageAdmin_RenameCallerKey(t *testing.T) {
	const key = "c1e5a7f3b0d24e6f9a8c7d6b5e4f3a2b"
	srv, store := fakeMagpiePage(t, key)
	a := NewPageAdmin(srv.URL, key, nil)
	ctx := context.Background()

	if _, err := a.EnsureCallerKey(ctx, "ls-token-1", "ssecret-1"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// 改名：老名消失、新名同 secret。
	if err := a.RenameCallerKey(ctx, "ls-token-1", "ls-u1-t1-alpha"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if v, ok := store.Load("ls-u1-t1-alpha"); !ok || v != "ssecret-1" {
		t.Fatalf("新名 secret 不符：%v %v", ok, v)
	}
	if _, ok := store.Load("ls-token-1"); ok {
		t.Fatal("旧名应已消失（原地改名，不是复制）")
	}
	// 幂等：重复 rename 同对名字成功。
	if err := a.RenameCallerKey(ctx, "ls-token-1", "ls-u1-t1-alpha"); err != nil {
		t.Fatalf("rename 幂等: %v", err)
	}
	// 新名已存在（同名可重复）：视为成功。
	if _, err := a.EnsureCallerKey(ctx, "other-key", "ssecret-2"); err != nil {
		t.Fatalf("add other: %v", err)
	}
	if err := a.RenameCallerKey(ctx, "ls-token-1", "other-key"); err != nil {
		t.Fatalf("目标名已存在应幂等成功: %v", err)
	}
	// 旧名不存在（已被删）：视为成功（回填竞态）。
	if err := a.RenameCallerKey(ctx, "gone-name", "ls-u1-t1-beta"); err != nil {
		t.Fatalf("旧名不存在应幂等成功: %v", err)
	}
}

// TestPageAdmin_RenameCallerKey_RejectsOverLimit 超 magpie 真机上限的名字在客户端就被拦（不发请求）。
func TestPageAdmin_RenameCallerKey_RejectsOverLimit(t *testing.T) {
	const key = "d2f6b8c4e1a35f7a9b8d7e6c5f4e3d2c"
	srv, _ := fakeMagpiePage(t, key)
	a := NewPageAdmin(srv.URL, key, nil)
	long := strings.Repeat("a", 121)
	if err := a.RenameCallerKey(context.Background(), "x", long); err == nil {
		t.Fatal("121 字符应被本地拦截（真机 400 同语义）")
	}
}
