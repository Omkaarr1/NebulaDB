![banner](https://github.com/user-attachments/assets/7211c646-aa09-47f4-89fc-e1bd0fd91faa)


# 🌌 NebulaDB

NebulaDB is a blazing-fast, lightweight, and versatile **disk-backed database** written in Go, built on top of [Fiber](https://github.com/gofiber/fiber) for high-performance HTTP operations. Designed to handle everything from JSON and text to files, blobs, images, and audio, NebulaDB offers ultra-low latency and high concurrency for your data storage needs.

---

## 🚀 Why NebulaDB?

Modern applications require databases that are fast, efficient, and easy to use without the overhead of complex systems. NebulaDB is built with simplicity and performance in mind:

- **Ultra-fast disk-backed persistent storage**: Leverage the power of native file system I/O for rapid reads and writes.
- **Organized Namespaces**: Group your data into namespaces, similar to folders, to keep your data organized.
- **Key-Value Style CRUD**: Effortlessly create, retrieve, update, and delete data using simple key-value operations.
- **Version Control**: Every write operation generates a new version of the file. With timestamped versioning, you can trace changes over time, access historical versions, and recover data if needed.
- **Concurrent Read-Write Operations**: Built-in concurrency mechanisms with read-write locks ensure safe and efficient access to data, even in highly concurrent environments.
- **Write-Ahead Logging (WAL)**: Logs every operation to guarantee durability and facilitate recovery.
- **LRU Caching**: Frequently accessed data is stored in an in-memory Least Recently Used (LRU) cache to boost read performance.
- **Data Replication**: Write operations are replicated across multiple files, ensuring higher availability and fault tolerance.
- **Prometheus Metrics Integration**: Comprehensive metrics expose key performance indicators such as request duration, operation counts, and system statistics.
- **Lightweight & Minimal Dependencies**: Designed to be minimalistic with no bloat, making it perfect for local-first applications and custom object stores.

---

## 🧠 Core Features in Detail

### **1. Disk-backed Storage**

- **Native File System I/O**: NebulaDB interacts directly with the file system, eliminating the need for additional database engines and reducing overhead.
- **Namespaces & Versioning**: Data is stored in namespace directories. Each stored item has a versioned file name based on a timestamp (e.g., `key_20250410_154532.gz`), allowing you to track every change.

### **2. Version Control**

- **Automatic Versioning**: Each write operation generates a new file with a unique version identifier. This timestamp-based versioning allows for:
  - **Historical Tracking**: Easily view and retrieve previous versions of your data.
  - **Audit Trails**: Maintain a log of changes for debugging or auditing purposes.
- **Version Listing & Retrieval**: APIs are provided to list all available versions for a specific key and to fetch any version on demand.

### **3. Read-Write Concurrency**

- **Efficient Read-Write Locks**: Internally, NebulaDB uses Go’s `sync.RWMutex` to manage concurrent access:
  - **Read Lock**: Multiple readers can access data simultaneously, which optimizes read-heavy workloads.
  - **Write Lock**: Ensures exclusive access during write operations to prevent data corruption.
- **Thread-Safe LRU Cache**: An in-memory Least Recently Used cache minimizes disk access by caching frequently requested data. It’s designed with thread safety in mind, using read-write locks to ensure consistent access in concurrent environments.

### **4. Write-Ahead Logging (WAL)**

- **Data Durability**: Every write operation is recorded in a Write-Ahead Log (`wal.log`) before being applied. This ensures:
  - **Recovery**: In the event of a failure, the WAL can be used to replay operations and restore the database to its last known state.
  - **Operation Auditing**: Track every operation (writes, updates, deletions) with detailed logs that include namespace, key, and data size.

### **5. Data Replication**

- **Redundancy**: Write operations are replicated across multiple files (default replica count is 3) to improve data availability and fault tolerance.
- **Asynchronous Replication**: Writes are handled concurrently across replicas using Go routines, ensuring that replication does not become a bottleneck for performance.

### **6. Compression**

- **GZIP Compression**: All data written to disk is compressed using GZIP, reducing disk space usage while ensuring efficient read/write operations. The corresponding decompression is applied during read operations, transparent to the user.

### **7. Prometheus Metrics**

- **Operational Metrics**: NebulaDB exposes metrics via a Prometheus endpoint (`/metrics`). These include:
  - **Operation Counters**: Track the total number of operations per type (write, read, etc.).
  - **Request Duration Histograms**: Measure the performance of various endpoints.
  - **System Statistics**: Monitor memory usage, garbage collection, and other runtime metrics.

---

## 🐳 Docker Quick Start

NebulaDB can be containerized and deployed in seconds without any external dependencies. Use the provided Dockerfile to build a lightweight container based on the `scratch` image.

```bash
# Build the container
docker build -t nebuladb .

# Run the container
docker run -p 3000:3000 nebuladb
```

---

## 📡 API Reference

All endpoints are under:  
`http://localhost:3000`

### **🔸 Store Data (POST)**

**POST** `/:namespace/:key`

#### Body (JSON | string | binary | file):
```json
{
  "name": "NebulaDB",
  "type": "demo"
}
```

**OR:** Raw file content (e.g., image, pdf, etc.)

#### Response:
```json
{
  "status": "ok",
  "version": "key_20250410_154532.gz"
}
```

---

### **🔹 Retrieve Data (GET)**

**GET** `/:namespace/:key`

#### Response (JSON or binary):
```json
{
  "name": "NebulaDB",
  "type": "demo"
}
```

For binary data, the API streams the raw content after decompressing it.

---

### **🔸 Update Data (PUT)**

**PUT** `/:namespace/:key`

Same body format as POST.

#### Response:
```json
{
  "status": "ok",
  "version": "key_20250410_154532.gz"
}
```

---

### **🔻 Delete Data (DELETE)**

**DELETE** `/:namespace/:key`

#### Response:
```json
{
  "status": "ok",
  "message": "Data deleted successfully"
}
```

---

### **🗂️ List Namespace Keys**

**GET** `/:namespace`

#### Response:
```json
{
  "namespace": "myspace",
  "keys": ["profile", "resume.pdf", "user.json"]
}
```

---

### **📝 List Versions**

**GET** `/versions/:namespace/:key`

#### Response:
```json
[
  "key_20250410_154532.gz",
  "key_20250410_154600.gz",
  "key_20250410_154615.gz"
]
```

---

### **📜 Retrieve Specific Version**

**GET** `/version/:namespace/:version`

#### Response:
```json
{
  "name": "NebulaDB",
  "type": "demo"
}
```

---

### **🗄 Metadata Retrieval**

**GET** `/metadata/:namespace/:key`

Provides smart metadata indexing for each stored key, including size, creation timestamp, and content type.

#### Response:
```json
[
  {
    "key": "demo",
    "version": "demo_20250410_154532.gz",
    "size": 1024,
    "created_at": "2025-04-10T15:45:32Z",
    "content_type": "application/json"
  }
]
```

---

### **📁 List All Namespaces**

**GET** `/namespaces`

#### Response:
```json
[
  "namespace1",
  "namespace2",
  "namespace3"
]
```

---

### **📄 List All Files in a Namespace**

**GET** `/namespace/:name/files`

#### Response:
```json
[
  "demo",
  "profile",
  "image"
]
```

---

## 🛠 Tech Stack & Internal Architecture

### **Tech Stack**

- **Go & Fiber**: High-performance HTTP framework for building efficient web applications.
- **Native File I/O**: Direct interaction with the file system for rapid data access.
- **Prometheus Client**: Real-time metrics monitoring for observability.
- **Concurrency Primitives**: Utilizes Go’s `sync.RWMutex` and Goroutines for managing concurrent operations and ensuring thread safety.
- **GZIP Compression**: Built-in compression for efficient storage and retrieval.

### **Internal Components**

#### **LRU Cache**
- **Purpose**: Cache frequently accessed data to minimize disk I/O.
- **Mechanism**: Implements a doubly linked list and a hash map for O(1) access. When the cache exceeds its predefined capacity (default: 100 items), it evicts the least recently used entry.
- **Thread Safety**: Uses read-write locks to allow concurrent access.

#### **Write-Ahead Logging (WAL)**
- **Purpose**: Ensures durability by logging each operation before execution.
- **Mechanism**: Each WAL entry includes operation type, namespace, key, and the data length, followed by the actual data.
- **Recovery**: In the event of a crash, the WAL can be replayed to restore the state.

#### **Data Replication**
- **Purpose**: Enhance data availability and reliability.
- **Mechanism**: Writes data to multiple replica files concurrently using Goroutines. The default replication factor is three, with additional replicas stored as variations of the original file name.

---

## 📌 Advanced Concepts

### **Version Control & Historical Data**

NebulaDB’s version control mechanism allows you to:
- **Trace Changes**: Each version is timestamped, making it easy to track modifications over time.
- **Rollback Capabilities**: Retrieve and restore previous versions if needed.
- **Audit and Analysis**: Detailed metadata records provide insights into data size, content type, and creation time.

### **Concurrency Management**

- **Read-Write Locks (`sync.RWMutex`)**:
  - **Readers**: Multiple readers can access the cache and disk concurrently, ensuring high throughput.
  - **Writers**: Exclusive access is granted for write operations, preventing race conditions and data corruption.
- **Goroutines**: Background tasks (like data replication and metrics collection) run concurrently to maximize performance.

### **Compression & Storage Efficiency**

- **GZIP Compression**:
  - **Storage Savings**: Compresses data before writing to disk, reducing storage costs.
  - **Performance**: Balances the overhead of compression with the benefit of reduced disk I/O.
- **Transparent Decompression**: On read operations, data is decompressed seamlessly before being delivered to the user.

---

## 🙌 Contributing

Contributions are welcome! Whether you find a bug, want to add a feature, or just want to discuss ideas, feel free to fork the repository, submit a pull request, or open an issue. Let’s build a faster, simpler, and more reliable future together.

---

## 📄 License

MIT — free to use, modify, and deploy.

---

**Built with caffeine and sarcasm by [@OMKAR](https://github.com/Omkaarr1)** ☕

Enjoy NebulaDB and let it power your data storage needs with simplicity and efficiency!
