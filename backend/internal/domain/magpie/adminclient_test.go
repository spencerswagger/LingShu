package magpie

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeMagpiePage 复刻页面鉴权：?k= 种 cookie；之后 /api/caller-keys* 需要 cookie。
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
