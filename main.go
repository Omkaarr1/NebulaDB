package main

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

const (
	basePath  = "./data"
	walPath   = "./wal.log"
	cacheSize = 100
)

var (
	opCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nebula_operations_total",
			Help: "Total number of operations by type",
		},
		[]string{"operation"},
	)
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "nebula_request_duration_seconds",
			Help:    "Histogram of request durations",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint"},
	)
)

func init() {
	prometheus.MustRegister(opCounter)
	prometheus.MustRegister(requestDuration)
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(":2112", nil)
	}()
}

type CacheItem struct {
	key   string
	value []byte
}

type LRUCache struct {
	capacity int
	list     *list.List
	items    map[string]*list.Element
	lock     sync.RWMutex
}

func NewLRUCache(cap int) *LRUCache {
	return &LRUCache{
		capacity: cap,
		list:     list.New(),
		items:    make(map[string]*list.Element),
	}
}

func (c *LRUCache) Get(key string) ([]byte, bool) {
	c.lock.RLock()
	elem, found := c.items[key]
	c.lock.RUnlock()
	if !found {
		return nil, false
	}
	c.lock.Lock()
	c.list.MoveToFront(elem)
	c.lock.Unlock()
	return elem.Value.(*CacheItem).value, true
}

func (c *LRUCache) Put(key string, value []byte) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if elem, ok := c.items[key]; ok {
		elem.Value.(*CacheItem).value = value
		c.list.MoveToFront(elem)
		return
	}
	if c.list.Len() >= c.capacity {
		tail := c.list.Back()
		if tail != nil {
			c.list.Remove(tail)
			delete(c.items, tail.Value.(*CacheItem).key)
		}
	}
	elem := c.list.PushFront(&CacheItem{key, value})
	c.items[key] = elem
}

var (
	cache = NewLRUCache(cacheSize)
	walMu sync.Mutex
)

func writeWAL(op, namespace, key string, data []byte) {
	walMu.Lock()
	defer walMu.Unlock()
	f, _ := os.OpenFile(walPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	entry := fmt.Sprintf("%s|%s|%s|%d\n", op, namespace, key, len(data))
	f.WriteString(entry)
	f.Write(data)
	opCounter.WithLabelValues(op).Inc()
}

func gzipCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	if err != nil {
		return nil, err
	}
	zw.Close()
	return buf.Bytes(), nil
}

func gzipDecompress(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func ensureDir(path string) error {
	return os.MkdirAll(path, os.ModePerm)
}

func versionedFilePath(namespace, key string) (string, string, error) {
	timestamp := time.Now().Format("20060102_150405")
	versionedName := fmt.Sprintf("%s_%s.gz", key, timestamp)
	nsPath := filepath.Join(basePath, namespace)
	if err := ensureDir(nsPath); err != nil {
		return "", "", err
	}
	filePath := filepath.Join(nsPath, versionedName)
	return filePath, versionedName, nil
}

func listVersions(namespace, key string) ([]string, error) {
	nsPath := filepath.Join(basePath, namespace)
	files, err := os.ReadDir(nsPath)
	if err != nil {
		return nil, err
	}
	var versions []string
	for _, f := range files {
		if strings.HasPrefix(f.Name(), key+"_") && strings.HasSuffix(f.Name(), ".gz") {
			versions = append(versions, f.Name())
		}
	}
	sort.Strings(versions)
	return versions, nil
}

func specificVersionPath(namespace, version string) (string, error) {
	path := filepath.Join(basePath, namespace, version)
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

func latestFilePath(namespace, key string) (string, error) {
	versions, err := listVersions(namespace, key)
	if err != nil || len(versions) == 0 {
		return "", os.ErrNotExist
	}
	return filepath.Join(basePath, namespace, versions[len(versions)-1]), nil
}

func main() {
	app := fiber.New()

	app.Post("/:namespace/:key", func(c *fiber.Ctx) error {
		start := time.Now()
		defer requestDuration.WithLabelValues("POST /:namespace/:key").Observe(time.Since(start).Seconds())

		ns := c.Params("namespace")
		key := c.Params("key")
		data := c.Body()

		compressed, err := gzipCompress(data)
		if err != nil {
			return c.Status(500).SendString("Compression failed")
		}

		filePath, versionedName, err := versionedFilePath(ns, key)
		if err != nil {
			return c.Status(500).SendString("Failed to build path")
		}

		writeWAL("WRITE", ns, key, compressed)

		err = os.WriteFile(filePath, compressed, 0644)
		if err != nil {
			return c.Status(500).SendString("Write failed")
		}
		cache.Put(ns+"/"+key, compressed)
		return c.JSON(fiber.Map{"status": "ok", "version": versionedName})
	})

	app.Get("/:namespace/:key", func(c *fiber.Ctx) error {
		start := time.Now()
		defer requestDuration.WithLabelValues("GET /:namespace/:key").Observe(time.Since(start).Seconds())

		ns := c.Params("namespace")
		key := c.Params("key")
		cacheKey := ns + "/" + key
		if val, ok := cache.Get(cacheKey); ok {
			data, _ := gzipDecompress(val)
			return c.Send(data)
		}
		filePath, err := latestFilePath(ns, key)
		if err != nil {
			return c.Status(404).SendString("Not found")
		}
		compressed, err := os.ReadFile(filePath)
		if err != nil {
			return c.Status(500).SendString("Read failed")
		}
		cache.Put(cacheKey, compressed)
		data, _ := gzipDecompress(compressed)
		return c.Send(data)
	})

	app.Get("/versions/:namespace/:key", func(c *fiber.Ctx) error {
		start := time.Now()
		defer requestDuration.WithLabelValues("GET /versions/:namespace/:key").Observe(time.Since(start).Seconds())

		ns := c.Params("namespace")
		key := c.Params("key")
		versions, err := listVersions(ns, key)
		if err != nil {
			return c.Status(500).SendString("Could not list versions")
		}
		return c.JSON(versions)
	})

	app.Get("/version/:namespace/:version", func(c *fiber.Ctx) error {
		start := time.Now()
		defer requestDuration.WithLabelValues("GET /version/:namespace/:version").Observe(time.Since(start).Seconds())

		ns := c.Params("namespace")
		version := c.Params("version")
		filePath, err := specificVersionPath(ns, version)
		if err != nil {
			return c.Status(404).SendString("Version not found")
		}
		compressed, err := os.ReadFile(filePath)
		if err != nil {
			return c.Status(500).SendString("Read failed")
		}
		data, err := gzipDecompress(compressed)
		if err != nil {
			return c.Status(500).SendString("Decompression failed")
		}
		return c.Send(data)
	})

	port := 3000
	fmt.Printf("✈ Running on http://localhost:%d\n", port)
	app.Listen(":" + strconv.Itoa(port))
}
