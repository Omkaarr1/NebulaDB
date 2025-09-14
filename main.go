// NebulaDB v2.3 — ultra-fast, user-friendly disk-backed KV + SQLite + Stats
// Namespaces • Short IDs • Short URLs • Atomic durable writes • LRU cache • ETag
// Endpoints: POST /sql/execute • GET /stats • KV routes (upload/get/list/delete)
//
// Build:
//   go mod init nebuladb
//   go get github.com/gofiber/fiber/v2 github.com/gofiber/fiber/v2/middleware/compress github.com/hashicorp/golang-lru/v2 github.com/glebarez/sqlite
//   go run main.go
//
// Env:
//   PORT=3000 DATA_DIR=./data CACHE_ITEMS=1024 MAX_CACHE_ITEM_BYTES=4194304
package main

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
    "os/signal"
    "syscall"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	_ "github.com/glebarez/sqlite" // driver name: "sqlite"
)

/* ============================ Config ============================ */

const (
	defaultBasePath          = "./data"
	defaultCacheItems        = 1024
	defaultMaxCacheItemBytes = 4 << 20 // 4 MiB
)

/* ============================ Types (KV) ======================== */

type VersionMetadata struct {
	Version      string    `json:"version"`
	Size         int       `json:"size"`
	CreatedAt    time.Time `json:"created_at"`
	ContentType  string    `json:"content_type"`
	Ext          string    `json:"ext"`
	PasswordHash string    `json:"password_hash,omitempty"`
}

type KeyMetadata struct {
	Key      string                     `json:"key"`
	Versions map[string]VersionMetadata `json:"versions"`
}

type NamespaceSnapshot struct {
	Keys map[string]KeyMetadata `json:"keys"`
}

type namespaceIndex struct {
	mu   sync.RWMutex
	keys map[string]*KeyMetadata // key -> meta
}

/* ============================ Store ============================ */

type CacheStats struct {
	Len          int    `json:"len"`
	MaxItemBytes int    `json:"max_item_bytes"`
	Adds         uint64 `json:"adds"`
	Gets         uint64 `json:"gets"`
	Hits         uint64 `json:"hits"`
	Misses       uint64 `json:"misses"`
	Removes      uint64 `json:"removes"`
}

type OpsCounters struct {
	Uploads        uint64 `json:"uploads"`
	GetLatest      uint64 `json:"get_latest"`
	GetVersion     uint64 `json:"get_version"`
	ListVersions   uint64 `json:"list_versions"`
	ListKeys       uint64 `json:"list_keys"`
	ListNamespaces uint64 `json:"list_namespaces"`
	Deletes        uint64 `json:"deletes"`
	SQLSingle      uint64 `json:"sql_single"`
	SQLBatch       uint64 `json:"sql_batch"`
}

type Store struct {
	basePath  string
	startTime time.Time

	idxMu sync.RWMutex
	idx   map[string]*namespaceIndex // ns -> index

	locks [256]sync.RWMutex

	cache             *lru.Cache[string, []byte]
	maxCacheItemBytes int

	cacheAdds    atomic.Uint64
	cacheGets    atomic.Uint64
	cacheHits    atomic.Uint64
	cacheMisses  atomic.Uint64
	cacheRemoves atomic.Uint64

	ops OpsCounters

	bytesWritten atomic.Uint64 // diagnostic only

	sql *SQLStore
}

/* ============================ SQL Store ========================= */

type SQLStore struct {
	db     *sql.DB
	parent *Store

	stmts   atomic.Uint64
	lastUse atomic.Int64 // unix ms
}

func (s *SQLStore) markUse() { s.stmts.Add(1); s.lastUse.Store(time.Now().UnixMilli()) }

func openSQLite(baseDir string) (*SQLStore, error) {
	dir := filepath.Join(baseDir, "_sql")
	if err := os.MkdirAll(dir, 0o755); err != nil { return nil, err }
	path := filepath.Join(dir, "nebula.db")

	dsn := fmt.Sprintf("file:%s", path) // glebarez/sqlite DSN
	db, err := sql.Open("sqlite", dsn)
	if err != nil { return nil, err }

	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(32)
	db.SetConnMaxLifetime(0)

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA temp_store=MEMORY;",
		"PRAGMA page_size=4096;",
		"PRAGMA cache_size=-20000;",   // ~20k pages in memory
		"PRAGMA mmap_size=268435456;", // 256 MiB (best-effort)
	}
	for _, p := range pragmas { _, _ = db.Exec(p) }

	if err := db.Ping(); err != nil { _ = db.Close(); return nil, err }
	return &SQLStore{db: db}, nil
}

type SQLStatement struct {
	SQL  string        `json:"sql"`
	Args []interface{} `json:"args"`
}

type SQLExecuteRequest struct {
	SQL       string         `json:"sql"`
	Args      []interface{}  `json:"args"`
	Batch     []SQLStatement `json:"batch"`
	Tx        bool           `json:"tx"`
	TimeoutMs int            `json:"timeout_ms"`
}

type SQLResult struct {
	OK           bool             `json:"ok"`
	Rows         []map[string]any `json:"rows,omitempty"`
	Columns      []string         `json:"columns,omitempty"`
	Count        int              `json:"count,omitempty"`
	RowsAffected int64            `json:"rows_affected,omitempty"`
	LastInsertID int64            `json:"last_insert_id,omitempty"`
	DurationMs   int64            `json:"duration_ms"`
	Error        string           `json:"error,omitempty"`
}

func (s *SQLStore) Close() { if s != nil && s.db != nil { _ = s.db.Close() } }

func isSelectLike(q string) bool {
	t := strings.TrimSpace(strings.ToUpper(q))
	return strings.HasPrefix(t, "SELECT") || strings.HasPrefix(t, "WITH ") || strings.HasPrefix(t, "PRAGMA")
}

func rowsToJSON(rows *sql.Rows) (cols []string, out []map[string]any, err error) {
	cols, err = rows.Columns()
	if err != nil { return nil, nil, err }
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals { ptrs[i] = &vals[i] }
		if err := rows.Scan(ptrs...); err != nil { return nil, nil, err }
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i]
			switch x := v.(type) {
			case []byte: m[c] = string(x)
			default:     m[c] = x
			}
		}
		out = append(out, m)
	}
	return cols, out, rows.Err()
}

func (s *SQLStore) execOne(ctx context.Context, st SQLStatement) (SQLResult, error) {
	start := time.Now()
	res := SQLResult{OK: true}
	if isSelectLike(st.SQL) {
		rows, err := s.db.QueryContext(ctx, st.SQL, st.Args...)
		if err != nil { return SQLResult{OK: false, Error: err.Error()}, nil }
		defer rows.Close()
		cols, data, err := rowsToJSON(rows)
		if err != nil { return SQLResult{OK: false, Error: err.Error()}, nil }
		res.Columns, res.Rows, res.Count = cols, data, len(data)
	} else {
		r, err := s.db.ExecContext(ctx, st.SQL, st.Args...)
		if err != nil { return SQLResult{OK: false, Error: err.Error()}, nil }
		if ra, err := r.RowsAffected(); err == nil { res.RowsAffected = ra }
		if li, err := r.LastInsertId(); err == nil { res.LastInsertID = li }
	}
	res.DurationMs = time.Since(start).Milliseconds()
	s.markUse()
	return res, nil
}

func (s *SQLStore) handleExecute(c *fiber.Ctx) error {
	if s == nil || s.db == nil { return c.Status(503).JSON(fiber.Map{"error": "sqlite_disabled"}) }

	var req SQLExecuteRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_json"})
	}

	timeout := time.Duration(0)
	if req.TimeoutMs > 0 { timeout = time.Duration(req.TimeoutMs) * time.Millisecond }
	stdCtx := context.Background()
	if timeout > 0 { ctx, _ := context.WithTimeout(stdCtx, timeout); stdCtx = ctx }

	var stmts []SQLStatement
	if len(req.Batch) > 0 {
		stmts = req.Batch
	} else if strings.TrimSpace(req.SQL) != "" {
		stmts = []SQLStatement{{SQL: req.SQL, Args: req.Args}}
	} else {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "missing_sql"})
	}

	useTx := req.Tx || len(stmts) > 1
	start := time.Now()

	if !useTx {
		out, _ := s.execOne(stdCtx, stmts[0])
		out.DurationMs = time.Since(start).Milliseconds()
		if s.parent != nil { atomic.AddUint64(&s.parent.ops.SQLSingle, 1) }
		return c.JSON(out)
	}

	tx, err := s.db.BeginTx(stdCtx, nil)
	if err != nil { return c.Status(500).JSON(fiber.Map{"error": err.Error()}) }

	results := make([]SQLResult, 0, len(stmts))
	for _, st := range stmts {
		if isSelectLike(st.SQL) {
			rows, err := tx.QueryContext(stdCtx, st.SQL, st.Args...)
			if err != nil { _ = tx.Rollback(); return c.Status(400).JSON(fiber.Map{"error": err.Error()}) }
			cols, data, err := rowsToJSON(rows); rows.Close()
			if err != nil { _ = tx.Rollback(); return c.Status(400).JSON(fiber.Map{"error": err.Error()}) }
			results = append(results, SQLResult{OK: true, Columns: cols, Rows: data, Count: len(data)})
		} else {
			r, err := tx.ExecContext(stdCtx, st.SQL, st.Args...)
			if err != nil { _ = tx.Rollback(); return c.Status(400).JSON(fiber.Map{"error": err.Error()}) }
			rr := SQLResult{OK: true}
			if ra, err := r.RowsAffected(); err == nil { rr.RowsAffected = ra }
			if li, err := r.LastInsertId(); err == nil { rr.LastInsertID = li }
			results = append(results, rr)
		}
		s.markUse()
	}
	if err := tx.Commit(); err != nil { return c.Status(500).JSON(fiber.Map{"error": err.Error()}) }

	totalMs := time.Since(start).Milliseconds()
	if s.parent != nil { atomic.AddUint64(&s.parent.ops.SQLBatch, 1) }
	return c.JSON(fiber.Map{"ok": true, "duration_ms": totalMs, "results": results})
}

/* ============================ Helpers (shared) ================== */

func getenvInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil { return n }
	}
	return def
}
func getenvStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" { return v }
	return def
}
func fnv32(s string) uint32 { h := fnv.New32a(); _, _ = h.Write([]byte(s)); return h.Sum32() }
func hashPassword(pw string) string {
	if pw == "" { return "" }
	s := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(s[:])
}
func constantTimeEqualHex(h1, h2 string) bool {
	if len(h1) != len(h2) { return false }
	return subtle.ConstantTimeCompare([]byte(h1), []byte(h2)) == 1
}
func extFromContentType(ct string) string {
	if ct == "" { return "" }
	exts, _ := mime.ExtensionsByType(ct)
	if len(exts) > 0 { return exts[0] }
	return ""
}
func cleanSegment(s string) string {
	s = strings.TrimSpace(s); if s == "" { return "default" }
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else { b.WriteByte('_') }
	}
	out := b.String(); if out == "" { return "default" }
	return out
}
func first512(b []byte) []byte { if len(b) <= 512 { return b }; return b[:512] }
func newVersionID() string {
	t := strconv.FormatInt(time.Now().UTC().UnixMilli(), 36)
	rnd := make([]byte, 4)
	if _, err := crand.Read(rnd); err != nil {
		n := time.Now().UTC().UnixNano()
		for i := 0; i < 4; i++ { rnd[i] = byte(n >> (i * 8)) }
	}
	return t + strings.ToLower(hex.EncodeToString(rnd))
}
func writeAtomically(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { return err }
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil { return err }
	if _, err := f.Write(data); err != nil { f.Close(); return err }
	if err := f.Sync(); err != nil { f.Close(); return err }
	if err := f.Close(); err != nil { return err }
	if err := os.Rename(tmp, path); err != nil { return err }
	if d, err := os.Open(dir); err == nil { _ = d.Sync(); _ = d.Close() }
	return nil
}
func cloneBytes(b []byte) []byte { cp := make([]byte, len(b)); copy(cp, b); return cp } // <-- added back

/* ============================ KV Indexing ======================= */

func newStore() *Store {
	base := getenvStr("DATA_DIR", defaultBasePath)
	cacheItems := getenvInt("CACHE_ITEMS", defaultCacheItems)
	maxBytes := getenvInt("MAX_CACHE_ITEM_BYTES", defaultMaxCacheItemBytes)

	_ = os.MkdirAll(base, 0o755)

	c, _ := lru.New[string, []byte](cacheItems)
	st := &Store{
		basePath:          base,
		idx:               make(map[string]*namespaceIndex),
		cache:             c,
		maxCacheItemBytes: maxBytes,
		startTime:         time.Now(),
	}
	if err := st.loadAllNamespaces(); err != nil { fmt.Printf("[warn] index load: %v\n", err) }

	sqlStore, err := openSQLite(base)
	if err != nil {
		fmt.Printf("[warn] sqlite open: %v\n", err)
	} else {
		sqlStore.parent = st
		st.sql = sqlStore
	}
	return st
}

func (s *Store) lockFor(ns, key string) *sync.RWMutex {
	h := fnv32(ns + "/" + key)
	return &s.locks[h%uint32(len(s.locks))]
}
func (s *Store) nsDir(ns string) string                   { return filepath.Join(s.basePath, ns) }
func (s *Store) keyDir(ns, key string) string             { return filepath.Join(s.basePath, ns, key) }
func (s *Store) filePath(ns, key, vid, ext string) string { return filepath.Join(s.basePath, ns, key, vid+ext) }
func (s *Store) metaPath(ns string) string                { return filepath.Join(s.basePath, ns, "metadata.json") }

func (s *Store) loadAllNamespaces() error {
	entries, err := os.ReadDir(s.basePath); if err != nil { return err }
	for _, e := range entries {
		if !e.IsDir() { continue }
		ns := e.Name()
		if err := s.loadNamespace(ns); err != nil { fmt.Printf("[warn] load ns %s: %v\n", ns, err) }
	}
	return nil
}
func (s *Store) loadNamespace(ns string) error {
	p := s.metaPath(ns)
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.idxMu.Lock(); s.idx[ns] = &namespaceIndex{keys: make(map[string]*KeyMetadata)}; s.idxMu.Unlock()
			return nil
		}
		return err
	}
	var snap NamespaceSnapshot
	if err := json.Unmarshal(b, &snap); err != nil { return err }
	ni := &namespaceIndex{keys: make(map[string]*KeyMetadata)}
	for k, v := range snap.Keys {
		if v.Versions == nil { v.Versions = make(map[string]VersionMetadata) }
		vv := v; vv.Key = cleanSegment(k)
		ni.keys[vv.Key] = &vv
	}
	s.idxMu.Lock(); s.idx[ns] = ni; s.idxMu.Unlock()
	return nil
}
func (s *Store) snapshotNamespace(ns string) error {
	s.idxMu.RLock(); ni, ok := s.idx[ns]; s.idxMu.RUnlock(); if !ok { return fmt.Errorf("namespace not found: %s", ns) }
	nsSnap := NamespaceSnapshot{Keys: make(map[string]KeyMetadata)}
	ni.mu.RLock()
	for k, meta := range ni.keys {
		km := KeyMetadata{Key: meta.Key, Versions: make(map[string]VersionMetadata, len(meta.Versions))}
		for ver, vm := range meta.Versions { km.Versions[ver] = vm }
		nsSnap.Keys[k] = km
	}
	ni.mu.RUnlock()
	b, _ := json.MarshalIndent(nsSnap, "", "  ")
	return writeAtomically(s.metaPath(ns), b)
}
func (s *Store) ensureNamespace(ns string) {
	s.idxMu.RLock(); _, ok := s.idx[ns]; s.idxMu.RUnlock(); if ok { return }
	s.idxMu.Lock(); if _, ok := s.idx[ns]; !ok { s.idx[ns] = &namespaceIndex{keys: make(map[string]*KeyMetadata)} }; s.idxMu.Unlock()
}

/* ============================ Cache wrappers ==================== */

func (s *Store) cacheGet(key string) ([]byte, bool) {
	s.cacheGets.Add(1)
	if b, ok := s.cache.Get(key); ok { s.cacheHits.Add(1); return b, true }
	s.cacheMisses.Add(1); return nil, false
}
func (s *Store) cacheAdd(key string, val []byte) { s.cacheAdds.Add(1); s.cache.Add(key, val) }
func (s *Store) cacheRemove(key string)          { s.cacheRemoves.Add(1); s.cache.Remove(key) }
func (s *Store) cacheKey(ns, key, vid string) string { return ns + "\x00" + key + "\x00" + vid }

/* ============================ KV Handlers ======================= */

func (s *Store) handleUpload(c *fiber.Ctx) error {
	ns := cleanSegment(c.Params("namespace"))
	key := cleanSegment(c.Params("key"))

	// reserve /sql/execute for SQL API
	if ns == "sql" && key == "execute" {
		return c.Status(405).JSON(fiber.Map{"error": "reserved_path", "hint": "Use POST /sql/execute"})
	}

	password := c.Get("X-Password")

	body := c.Body()
	if len(body) == 0 {
		if r := c.Context().RequestBodyStream(); r != nil {
			buf := bufio.NewReader(r); b, _ := io.ReadAll(buf); body = b
		}
	}

	ct := c.Get("Content-Type"); if ct == "" { ct = http.DetectContentType(first512(body)) }
	ext := extFromContentType(ct); if ext == "" { ext = ".bin" }

	vid := newVersionID()
	path := s.filePath(ns, key, vid, ext)

	s.ensureNamespace(ns)
	lk := s.lockFor(ns, key); lk.Lock(); defer lk.Unlock()

	if err := writeAtomically(path, body); err != nil { return c.Status(500).JSON(fiber.Map{"error": "write_failed"}) }

	s.idxMu.RLock(); ni := s.idx[ns]; s.idxMu.RUnlock()
	ni.mu.Lock()
	km, ok := ni.keys[key]; if !ok { km = &KeyMetadata{Key: key, Versions: make(map[string]VersionMetadata)}; ni.keys[key] = km }
	vm := VersionMetadata{Version: vid, Size: len(body), CreatedAt: time.Now().UTC(), ContentType: ct, Ext: ext, PasswordHash: hashPassword(password)}
	km.Versions[vid] = vm
	ni.mu.Unlock()

	if err := s.snapshotNamespace(ns); err != nil { fmt.Printf("[warn] snapshot %s: %v\n", ns, err) }

	if len(body) <= s.maxCacheItemBytes { s.cacheAdd(s.cacheKey(ns, key, vid), cloneBytes(body)) }

	s.bytesWritten.Add(uint64(len(body))); atomic.AddUint64(&s.ops.Uploads, 1)

	return c.JSON(fiber.Map{"status": "ok", "key": key, "version": vid, "url": fmt.Sprintf("/u/%s/%s/%s", ns, key, vid), "latest": fmt.Sprintf("/u/%s/%s", ns, key)})
}

func (s *Store) latestVersion(ni *namespaceIndex, key string) (VersionMetadata, bool) {
	ni.mu.RLock(); km, ok := ni.keys[key]; ni.mu.RUnlock(); if !ok || len(km.Versions) == 0 { return VersionMetadata{}, false }
	var latest VersionMetadata; var ts int64
	for _, v := range km.Versions { if t := v.CreatedAt.UnixNano(); t > ts { ts, latest = t, v } }
	return latest, true
}

func (s *Store) handleGetLatest(c *fiber.Ctx) error {
	ns := cleanSegment(c.Params("namespace")); key := cleanSegment(c.Params("key")); password := c.Get("X-Password")

	s.idxMu.RLock(); ni, ok := s.idx[ns]; s.idxMu.RUnlock(); if !ok { return c.Status(404).SendString("namespace not found") }

	lk := s.lockFor(ns, key); lk.RLock(); defer lk.RUnlock()
	meta, ok := s.latestVersion(ni, key); if !ok { return c.Status(404).SendString("key not found") }

	if meta.PasswordHash != "" && !constantTimeEqualHex(meta.PasswordHash, hashPassword(password)) { return c.Status(403).SendString("unauthorized: password required") }

	etag := `"` + meta.Version + `"`; if inm := c.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) { return c.Status(304).SendString("") }

	c.Set("ETag", etag); c.Set("Last-Modified", meta.CreatedAt.UTC().Format(http.TimeFormat)); c.Set("X-Version", meta.Version); c.Type(meta.ContentType)

	if b, ok := s.cacheGet(s.cacheKey(ns, key, meta.Version)); ok { atomic.AddUint64(&s.ops.GetLatest, 1); return c.Send(b) }
	atomic.AddUint64(&s.ops.GetLatest, 1); return c.SendFile(s.filePath(ns, key, meta.Version, meta.Ext))
}

func (s *Store) handleGetVersion(c *fiber.Ctx) error {
	ns := cleanSegment(c.Params("namespace")); key := cleanSegment(c.Params("key")); vid := c.Params("version"); password := c.Get("X-Password")

	s.idxMu.RLock(); ni, ok := s.idx[ns]; s.idxMu.RUnlock(); if !ok { return c.Status(404).SendString("namespace not found") }

	lk := s.lockFor(ns, key); lk.RLock(); defer lk.RUnlock()
	ni.mu.RLock(); km, ok := ni.keys[key]; if !ok { ni.mu.RUnlock(); return c.Status(404).SendString("key not found") }
	meta, ok := km.Versions[vid]; ni.mu.RUnlock(); if !ok { return c.Status(404).SendString("version not found") }

	if meta.PasswordHash != "" && !constantTimeEqualHex(meta.PasswordHash, hashPassword(password)) { return c.Status(403).SendString("unauthorized: password required") }

	etag := `"` + meta.Version + `"`; if inm := c.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) { return c.Status(304).SendString("") }

	c.Set("ETag", etag); c.Set("Last-Modified", meta.CreatedAt.UTC().Format(http.TimeFormat)); c.Set("X-Version", meta.Version); c.Type(meta.ContentType)

	if b, ok := s.cacheGet(s.cacheKey(ns, key, meta.Version)); ok { atomic.AddUint64(&s.ops.GetVersion, 1); return c.Send(b) }
	atomic.AddUint64(&s.ops.GetVersion, 1); return c.SendFile(s.filePath(ns, key, meta.Version, meta.Ext))
}

func (s *Store) handleListVersions(c *fiber.Ctx) error {
	ns := cleanSegment(c.Params("namespace")); key := cleanSegment(c.Params("key"))

	s.idxMu.RLock(); ni, ok := s.idx[ns]; s.idxMu.RUnlock(); if !ok { return c.Status(404).SendString("namespace not found") }

	ni.mu.RLock(); km, ok := ni.keys[key]; if !ok { ni.mu.RUnlock(); return c.Status(404).SendString("key not found") }
	out := make([]VersionMetadata, 0, len(km.Versions)); for _, v := range km.Versions { out = append(out, v) }; ni.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	type vout struct{ Version, Url string; CreatedAt time.Time; Size int; ContentType, Ext string }
	res := make([]vout, 0, len(out))
	for _, vm := range out { res = append(res, vout{Version: vm.Version, Url: fmt.Sprintf("/u/%s/%s/%s", ns, key, vm.Version), CreatedAt: vm.CreatedAt, Size: vm.Size, ContentType: vm.ContentType, Ext: vm.Ext}) }
	atomic.AddUint64(&s.ops.ListVersions, 1); return c.JSON(res)
}

func (s *Store) handleDelete(c *fiber.Ctx) error {
	ns := cleanSegment(c.Params("namespace")); key := cleanSegment(c.Params("key")); password := c.Get("X-Password")

	s.idxMu.RLock(); ni, ok := s.idx[ns]; s.idxMu.RUnlock(); if !ok { return c.Status(404).SendString("namespace not found") }

	lk := s.lockFor(ns, key); lk.Lock(); defer lk.Unlock()

	ni.mu.RLock(); km, ok := ni.keys[key]; ni.mu.RUnlock(); if !ok { return c.Status(404).SendString("key not found") }

	latest, ok := s.latestVersion(ni, key); if !ok { return c.Status(404).SendString("key not found") }
	if latest.PasswordHash != "" && !constantTimeEqualHex(latest.PasswordHash, hashPassword(password)) { return c.Status(403).SendString("unauthorized: incorrect password") }

	for ver, vm := range km.Versions {
		_ = os.Remove(s.filePath(ns, key, ver, vm.Ext))
		s.cacheRemove(s.cacheKey(ns, key, ver))
		if d, err := os.Open(s.keyDir(ns, key)); err == nil { _ = d.Sync(); _ = d.Close() }
	}

	ni.mu.Lock(); delete(ni.keys, key); ni.mu.Unlock()
	if err := s.snapshotNamespace(ns); err != nil { fmt.Printf("[warn] snapshot %s: %v\n", ns, err) }

	atomic.AddUint64(&s.ops.Deletes, 1)
	return c.JSON(fiber.Map{"status": "ok", "message": "deleted key and all versions", "namespace": ns, "key": key})
}

/* ====== List Namespaces & List Keys ====== */

func (s *Store) handleListNamespaces(c *fiber.Ctx) error {
	s.idxMu.RLock()
	ns := make([]string, 0, len(s.idx))
	for name := range s.idx { ns = append(ns, name) }
	s.idxMu.RUnlock()
	sort.Strings(ns)
	atomic.AddUint64(&s.ops.ListNamespaces, 1)
	return c.JSON(fiber.Map{"namespaces": ns})
}

func (s *Store) handleListKeys(c *fiber.Ctx) error {
	ns := cleanSegment(c.Params("namespace"))
	s.idxMu.RLock(); ni, ok := s.idx[ns]; s.idxMu.RUnlock()
	if !ok {
		return c.JSON(fiber.Map{"namespace": ns, "keys": []any{}})
	}
	ni.mu.RLock()
	keys := make([]string, 0, len(ni.keys))
	for k := range ni.keys { keys = append(keys, k) }
	ni.mu.RUnlock()
	sort.Strings(keys)

	type kout struct{ Key, LatestURL string }
	out := make([]kout, 0, len(keys))
	for _, k := range keys { out = append(out, kout{Key: k, LatestURL: fmt.Sprintf("/u/%s/%s", ns, k)}) }
	atomic.AddUint64(&s.ops.ListKeys, 1)
	return c.JSON(fiber.Map{"namespace": ns, "keys": out})
}

/* ============================ Stats ============================= */

type DiskStats struct {
	DataDir        string `json:"data_dir"`
	TotalBytes     uint64 `json:"total_bytes"`      // best-effort; 0 on unsupported platforms
	FreeBytes      uint64 `json:"free_bytes"`       // best-effort; 0 on unsupported platforms
	UsedBytesStore uint64 `json:"used_bytes_store"` // sum of files under DATA_DIR
}
type SQLStats struct {
	Enabled    bool   `json:"enabled"`
	DBPath     string `json:"db_path,omitempty"`
	PageSize   int64  `json:"page_size,omitempty"`
	PageCount  int64  `json:"page_count,omitempty"`
	FreeList   int64  `json:"freelist_count,omitempty"`
	WALBytes   int64  `json:"wal_bytes,omitempty"`
	FileBytes  int64  `json:"file_bytes,omitempty"`
	Statements uint64 `json:"statements"`
	LastUseMs  int64  `json:"last_use_ms"`
}
type KeySpaceStats struct {
	Namespaces int `json:"namespaces"`
	Keys       int `json:"keys"`
	Versions   int `json:"versions"`
}
type RuntimeStats struct {
	GoVersion     string `json:"go_version"`
	NumCPU        int    `json:"num_cpu"`
	NumGoroutine  int    `json:"num_goroutine"`
	UptimeSec     int64  `json:"uptime_sec"`
	StartTime     string `json:"start_time"`
	MemAlloc      uint64 `json:"mem_alloc"`
	MemSys        uint64 `json:"mem_sys"`
	HeapAlloc     uint64 `json:"heap_alloc"`
	HeapSys       uint64 `json:"heap_sys"`
	GCNum         uint32 `json:"gc_num"`
	LastGCPauseNs uint64 `json:"last_gc_pause_ns"`
}
type StatsResponse struct {
	Runtime      RuntimeStats  `json:"runtime"`
	Disk         DiskStats     `json:"disk"`
	Keyspace     KeySpaceStats `json:"keyspace"`
	Cache        CacheStats    `json:"cache"`
	Ops          OpsCounters   `json:"ops"`
	BytesWritten uint64        `json:"bytes_written_since_start"`
	SQLite       SQLStats      `json:"sqlite"`
	Endpoints    []string      `json:"endpoints"`
}

func (s *Store) computeDiskUsage() (uint64, error) {
	var sum uint64
	err := filepath.WalkDir(s.basePath, func(path string, d os.DirEntry, err error) error {
		if err != nil { return nil }
		if d.IsDir() { return nil }
		if info, e := d.Info(); e == nil { sum += uint64(info.Size()) }
		return nil
	})
	return sum, err
}
func fsTotals(_ string) (total, free uint64, err error) { return 0, 0, nil } // cross-platform best-effort

func (s *Store) keyspaceStats() KeySpaceStats {
	ks := KeySpaceStats{}
	s.idxMu.RLock(); ks.Namespaces = len(s.idx)
	for _, ni := range s.idx {
		ni.mu.RLock()
		ks.Keys += len(ni.keys)
		for _, km := range ni.keys { ks.Versions += len(km.Versions) }
		ni.mu.RUnlock()
	}
	s.idxMu.RUnlock()
	return ks
}
func (s *Store) cacheStats() CacheStats {
	return CacheStats{
		Len:          s.cache.Len(),
		MaxItemBytes: s.maxCacheItemBytes,
		Adds:         s.cacheAdds.Load(),
		Gets:         s.cacheGets.Load(),
		Hits:         s.cacheHits.Load(),
		Misses:       s.cacheMisses.Load(),
		Removes:      s.cacheRemoves.Load(),
	}
}
func (s *Store) sqlStats() SQLStats {
	st := SQLStats{Enabled: s.sql != nil}
	if s.sql == nil { return st }
	dbDir := filepath.Join(s.basePath, "_sql")
	dbPath := filepath.Join(dbDir, "nebula.db")
	st.DBPath = dbPath

	var ps, pc, fl int64
	_ = s.sql.db.QueryRow("PRAGMA page_size;").Scan(&ps)
	_ = s.sql.db.QueryRow("PRAGMA page_count;").Scan(&pc)
	_ = s.sql.db.QueryRow("PRAGMA freelist_count;").Scan(&fl)
	st.PageSize, st.PageCount, st.FreeList = ps, pc, fl

	if fi, err := os.Stat(dbPath); err == nil { st.FileBytes = fi.Size() }
	if fi, err := os.Stat(dbPath + "-wal"); err == nil { st.WALBytes = fi.Size() }

	st.Statements = s.sql.stmts.Load()
	st.LastUseMs = s.sql.lastUse.Load()
	return st
}

func (s *Store) handleStats(c *fiber.Ctx) error {
	var m runtime.MemStats; runtime.ReadMemStats(&m)
	rt := RuntimeStats{
		GoVersion: runtime.Version(), NumCPU: runtime.NumCPU(), NumGoroutine: runtime.NumGoroutine(),
		UptimeSec: int64(time.Since(s.startTime).Seconds()), StartTime: s.startTime.UTC().Format(time.RFC3339),
		MemAlloc: m.Alloc, MemSys: m.Sys, HeapAlloc: m.HeapAlloc, HeapSys: m.HeapSys,
		GCNum: m.NumGC, LastGCPauseNs: m.PauseNs[(m.NumGC+255)%256],
	}

	used, _ := s.computeDiskUsage()
	tot, free, _ := fsTotals(s.basePath)
	ds := DiskStats{DataDir: s.basePath, TotalBytes: tot, FreeBytes: free, UsedBytesStore: used}

	resp := StatsResponse{
		Runtime: rt, Disk: ds, Keyspace: s.keyspaceStats(), Cache: s.cacheStats(),
		Ops: s.ops, BytesWritten: s.bytesWritten.Load(), SQLite: s.sqlStats(),
		Endpoints: []string{
			"POST   /sql/execute",
			"GET    /stats",
			"POST   /:namespace/:key",
			"GET    /u/:namespace/:key",
			"GET    /u/:namespace/:key/:version",
			"GET    /versions/:namespace/:key",
			"GET    /namespace",
			"GET    /:namespace",
			"DELETE /:namespace/:key",
			"GET    /:namespace/:key (legacy)",
			"GET    /version/:namespace/:version (legacy)",
		},
	}
	return c.JSON(resp)
}

/* ============================ Legacy routing ==================== */

func (s *Store) legacyGet(c *fiber.Ctx) error { // GET /:namespace/:key
	c.Path("/u/" + cleanSegment(c.Params("namespace")) + "/" + cleanSegment(c.Params("key")))
	return s.handleGetLatest(c)
}
func (s *Store) legacyGetVersion(c *fiber.Ctx) error { // GET /version/:namespace/:version?key=...
	ns := cleanSegment(c.Params("namespace")); vid := c.Params("version"); key := cleanSegment(c.Query("key"))
	c.Path("/u/" + ns + "/" + key + "/" + vid)
	return s.handleGetVersion(c)
}

/* ============================ Main ============================== */

// add near your other helpers
func getenvBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" { return def }
	switch strings.ToLower(v) {
	case "1","true","yes","y","on": return true
	case "0","false","no","n","off": return false
	default: return def
	}
}


func main() {
	store := newStore()

	app := fiber.New(fiber.Config{
		Prefork:       getenvBool("PREFORK", false), // default OFF; set PREFORK=1 to enable
		ReadTimeout:   30 * time.Second,
		WriteTimeout:  30 * time.Second,
		IdleTimeout:   60 * time.Second,
		CaseSensitive: true,
		StrictRouting: true,
	})

	app.Use(compress.New(compress.Config{Level: compress.LevelBestSpeed}))

	// Register specific routes BEFORE generic KV to avoid collisions.
	if store.sql != nil {
		app.Post("/sql/execute", store.sql.handleExecute)
	}
	app.Get("/stats", store.handleStats)

	// KV
	app.Post("/:namespace/:key", store.handleUpload)
	app.Get("/u/:namespace/:key", store.handleGetLatest)
	app.Get("/u/:namespace/:key/:version", store.handleGetVersion)
	app.Get("/versions/:namespace/:key", store.handleListVersions)
	app.Get("/namespace", store.handleListNamespaces)
	app.Get("/:namespace", store.handleListKeys)
	app.Delete("/:namespace/:key", store.handleDelete)

	// Legacy
	app.Get("/:namespace/:key", store.legacyGet)
	app.Get("/version/:namespace/:version", store.legacyGetVersion)

	// Graceful shutdown on SIGINT/SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-quit
		_ = app.Shutdown()
	}()

	port := getenvInt("PORT", 3000)
	addr := ":" + strconv.Itoa(port)
	fmt.Printf("\n🚀 NebulaDB v2.3 at http://localhost:%d  data=%s  (short URLs + /sql/execute + /stats)\n", port, store.basePath)

	if err := app.Listen(addr); err != nil {
		// Don't crash on normal close paths (common with prefork/containers)
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "server closed") &&
			!strings.Contains(msg, "use of closed network connection") &&
			!strings.Contains(msg, "listener closed") {
			fmt.Printf("[fatal] listen error: %v\n", err)
			os.Exit(1)
		}
	}
}
