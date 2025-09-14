![banner](https://github.com/user-attachments/assets/7211c646-aa09-47f4-89fc-e1bd0fd91faa)

# 🌌 NebulaDB v2.3

NebulaDB is a blazing-fast, lightweight, **disk-backed key/value store** written in Go (Fiber). It handles JSON, text, and arbitrary binary blobs with **ultra-low latency**, simple APIs, and zero external dependencies. New in v2.3: **short URLs & version IDs**, **atomic durable writes**, **built-in SQLite execution**, and a rich **/stats** endpoint.

---

## 🚀 Why NebulaDB?

Modern apps need a store that’s fast, predictable, and easy. NebulaDB focuses on:

* **Simplicity** – intuitive HTTP APIs, no orchestration required.
* **Speed** – short code paths, in-memory LRU cache, ETag support.
* **Durability** – atomic writes (temp → fsync → rename → dir fsync).
* **Observability** – a detailed **/stats** view of everything.

---

## ✨ What’s new in v2.3

* **Short URLs**

  * Latest value: `GET /u/:namespace/:key`
  * Specific version: `GET /u/:namespace/:key/:version`
* **Short version IDs** (base36 time + random), human-ish and compact.
* **Atomic on-disk layout**: `data/<ns>/<key>/<version>.<ext>`
* **ETag / If-None-Match** support for cache-friendly reads.
* **Built-in SQLite**: `POST /sql/execute` for ultra-low-latency queries & batches.
* **Deep /stats** endpoint: runtime, mem, cache hits, keyspace counts, disk usage, ops counters, and SQLite internals.
* **Prefork toggle** via `PREFORK=1` when you want multiple workers.

> 🔐 Optional per-version protection: send `X-Password` on **upload** to lock a key; reads/deletes will require the same header (verified by secure hash).

---

## 🐳 Docker Quick Start

```bash
# Build (if you’re packaging locally)
docker build -t nebuladb:alpine .

# Run
docker run --rm -p 3000:3000 -v "$(pwd)/data:/data" \
  -e PORT=3000 -e DATA_DIR=/data -e PREFORK=0 \
  --name nebuladb nebuladb:alpine
```

**Defaults**

* Port: `3000`
* Data dir: `/data`
* Optional env: `CACHE_ITEMS` (default 1024), `MAX_CACHE_ITEM_BYTES` (default 4MiB), `PREFORK` (default 0)

---

# 📡 API Reference (Complete)

Base URL: `http://localhost:3000`

### Common headers & behaviors

* **`Content-Type`**: auto-detected on upload; echoed on reads.
* **`X-Password`** (optional on upload): protects the key; same header required to read or delete.
* **`ETag`** on reads: you can send **`If-None-Match`** to get `304 Not Modified`.
* **`X-Version`** on reads: returns the version ID you got.
* **Version IDs**: short (base36 time + 4 random bytes), e.g. `l8g7ma1f1b2c`.

---

## 1) Key/Value (KV) routes

### Create/Update (write a new version)

**POST** `/:namespace/:key`
Body: raw binary / JSON / text
Optional headers: `X-Password: <secret>`

**Response**

```json
{
  "status": "ok",
  "key": "profile",
  "version": "l8g7ma1f1b2c",
  "url": "/u/myspace/profile/l8g7ma1f1b2c",
  "latest": "/u/myspace/profile"
}
```

**Notes**

* Writes are **atomic** (tmp → fsync → rename → dir fsync).
* The path `ns=sql` + `key=execute` is **reserved** for the SQL API; you’ll get `405` if you try to upload there.

---

### Read (latest version)

**GET** `/u/:namespace/:key`
Optional headers:

* `X-Password: <secret>` (if protected)
* `If-None-Match: "l8g7ma1f1b2c"` (returns `304` if unchanged)

**Response**

* Body: raw blob
* Headers: `Content-Type`, `ETag`, `X-Version`, `Last-Modified`

---

### Read (specific version)

**GET** `/u/:namespace/:key/:version`
Same behavior as “Read latest,” but fixed to `:version`.

---

### List versions for a key

**GET** `/versions/:namespace/:key`

**Response**

```json
[
  {
    "Version": "l8g7ma1f1b2c",
    "Url": "/u/myspace/profile/l8g7ma1f1b2c",
    "CreatedAt": "2025-03-30T12:34:56Z",
    "Size": 1024,
    "ContentType": "application/json",
    "Ext": ".json"
  }
]
```

---

### List all namespaces

**GET** `/namespace`

**Response**

```json
{ "namespaces": ["myspace", "images", "notes"] }
```

---

### List keys in a namespace

**GET** `/:namespace`

**Response**

```json
{
  "namespace": "myspace",
  "keys": [
    { "Key": "profile", "LatestURL": "/u/myspace/profile" },
    { "Key": "avatar",  "LatestURL": "/u/myspace/avatar" }
  ]
}
```

---

### Delete a key (all versions)

**DELETE** `/:namespace/:key`
Optional header: `X-Password: <secret>` (required if protected)

**Response**

```json
{
  "status": "ok",
  "message": "deleted key and all versions",
  "namespace": "myspace",
  "key": "profile"
}
```

---

## 2) SQL execution (SQLite)

### Execute a single statement

**POST** `/sql/execute`
**Request**

```json
{
  "sql": "SELECT 1 AS ok"
}
```

**Response**

```json
{
  "ok": true,
  "duration_ms": 1,
  "rows": [{"ok": 1}],
  "columns": ["ok"],
  "count": 1
}
```

### Execute a batch (transaction)

**POST** `/sql/execute`
**Request**

```json
{
  "tx": true,
  "batch": [
    { "sql": "CREATE TABLE IF NOT EXISTS notes(id INTEGER PRIMARY KEY, txt TEXT)" },
    { "sql": "INSERT INTO notes(txt) VALUES (?)", "args": ["hello nebula"] },
    { "sql": "SELECT * FROM notes" }
  ]
}
```

**Response**

```json
{
  "ok": true,
  "duration_ms": 3,
  "results": [
    { "ok": true },                                  // CREATE
    { "ok": true, "rows_affected": 1, "last_insert_id": 1 }, // INSERT
    { "ok": true, "columns": ["id","txt"], "rows": [{"id":1,"txt":"hello nebula"}], "count": 1 } // SELECT
  ]
}
```

**Options**

* `"timeout_ms": 5000` to bound query time.
* You can also use single-statement form: `{"sql":"...", "args":[...]}`.

---

## 3) Stats & introspection

### Everything the server is doing (runtime, cache, keyspace, disk, ops, SQLite)

**GET** `/stats`

**Response (shape)**

```json
{
  "runtime": { "go_version": "...", "num_cpu": 8, "num_goroutine": 32, "uptime_sec": 123, ... },
  "disk":    { "data_dir": "/data", "total_bytes": 0, "free_bytes": 0, "used_bytes_store": 1234567 },
  "keyspace":{ "namespaces": 2, "keys": 5, "versions": 12 },
  "cache":   { "len": 42, "max_item_bytes": 4194304, "adds": 200, "gets": 1000, "hits": 800, "misses": 200, "removes": 10 },
  "ops":     { "uploads": 10, "get_latest": 300, "get_version": 50, "list_versions": 20, "list_keys": 15, "list_namespaces": 2, "deletes": 1, "sql_single": 5, "sql_batch": 2 },
  "bytes_written_since_start": 123456,
  "sqlite":  { "enabled": true, "db_path": "/data/_sql/nebula.db", "page_size": 4096, "page_count": 123, "freelist_count": 0, "wal_bytes": 0, "file_bytes": 8192, "statements": 7, "last_use_ms": 1700000000000 },
  "endpoints": [
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
    "GET    /version/:namespace/:version (legacy)"
  ]
}
```

---

## 4) Legacy (compat) routes

> These exist for older clients. Prefer the short `/u/...` routes.

### Legacy read latest

**GET** `/:namespace/:key`
(Equivalent to `GET /u/:namespace/:key`)

### Legacy read specific version

**GET** `/version/:namespace/:version?key=<key>`
(Equivalent to `GET /u/:namespace/:key/:version`)

---

## Status codes you’ll see

* `200 OK` – success (reads/lists/SQL)
* `201` not used; writes return `200` with a JSON body
* `304 Not Modified` – when `If-None-Match` matches `ETag`
* `400 Bad Request` – invalid JSON or bad SQL
* `403 Forbidden` – wrong/missing `X-Password` on protected keys
* `404 Not Found` – unknown namespace/key/version
* `405 Method Not Allowed` – reserved paths (e.g., KV upload to `/sql/execute`)
* `500` – unexpected server errors

---

## Example curl snippets

**Upload JSON**

```bash
curl -X POST http://localhost:3000/myspace/profile \
  -H "Content-Type: application/json" \
  -d '{"name":"NebulaDB","type":"demo"}'
```

**Read latest**

```bash
curl -i http://localhost:3000/u/myspace/profile
```

**Read with cache validator**

```bash
curl -i http://localhost:3000/u/myspace/profile \
  -H 'If-None-Match: "l8g7ma1f1b2c"'
```

**List versions**

```bash
curl http://localhost:3000/versions/myspace/profile
```

**Run SQL (single)**

```bash
curl -X POST http://localhost:3000/sql/execute \
  -H "Content-Type: application/json" \
  -d '{"sql":"SELECT 1 AS ok"}'
```

**Run SQL (batch in a tx)**

```bash
curl -X POST http://localhost:3000/sql/execute \
  -H "Content-Type: application/json" \
  -d '{"tx":true,"batch":[{"sql":"CREATE TABLE IF NOT EXISTS notes(id INTEGER PRIMARY KEY, txt TEXT)"},{"sql":"INSERT INTO notes(txt) VALUES (?)","args":["hello"]},{"sql":"SELECT * FROM notes"}]}'
```

**Stats**

```bash
curl http://localhost:3000/stats
```

**Delete key**

```bash
curl -X DELETE http://localhost:3000/myspace/profile
```

## 🧠 Internals (quick)

* **On-disk**: `data/<namespace>/<key>/<version>.<ext>`
* **Version IDs**: base36 timestamp + random bytes (short & monotonic-ish)
* **Durability**: temp write → `fsync` → `rename` → parent dir `fsync`
* **Cache**: LRU (size-limited per item), conditional GETs via ETag
* **Security**: optional `X-Password` hashed per version; deletes validate against latest

---

## 🗺 Roadmap

* WAL + crash recovery / compaction
* Transparent compression (e.g., zstd)
* Prometheus `/metrics`
* HTTP Range / resumable uploads

---

## 🙌 Contributing

Issues, PRs, and ideas welcome. Keep it simple, keep it fast.

## 📄 License

MIT

---

**Built with caffeine and sarcasm by [@OMKAR](https://github.com/Omkaarr1)** ☕

Enjoy NebulaDB and let it power your data with **short URLs, fast reads, and low drama**.
