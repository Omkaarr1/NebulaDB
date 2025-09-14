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

## 📡 API Reference (current)

Base URL: `http://localhost:3000`

### Store / Update (latest version)

**POST** `/:namespace/:key`
Body: raw binary, JSON, or text
Optional: `X-Password: <secret>` to protect this key (checked on reads/deletes)

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

### Get latest

**GET** `/u/:namespace/:key`

* Returns raw blob with `Content-Type` and `ETag` headers
* Optional: `If-None-Match` for 304s
* Optional: `X-Password` if key is protected

### Get a specific version

**GET** `/u/:namespace/:key/:version`

* Same behaviors/headers as above

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

### List namespaces

**GET** `/namespace`
**Response**

```json
{ "namespaces": ["myspace", "images", "notes"] }
```

### List keys in a namespace

**GET** `/:namespace`
**Response**

```json
{
  "namespace": "myspace",
  "keys": [
    { "Key": "profile", "LatestURL": "/u/myspace/profile" }
  ]
}
```

### Delete a key (all versions)

**DELETE** `/:namespace/:key`

* If the latest version was uploaded with `X-Password`, the same header is required here.

**Response**

```json
{ "status": "ok", "message": "deleted key and all versions", "namespace": "myspace", "key": "profile" }
```

### Execute SQL (SQLite)

**POST** `/sql/execute`
Single:

```json
{ "sql": "SELECT 1 AS ok" }
```

Batch (transaction):

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

**Response (example)**

```json
{ "ok": true, "duration_ms": 2, "results": [ { "rows": [{"id":1,"txt":"hello nebula"}], "count": 1 } ] }
```

### Stats (everything at a glance)

**GET** `/stats`
Includes runtime (Go/mem/GC), cache stats (hits/misses), keyspace counts, disk usage, ops counters, and SQLite page/cache info.

---

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
