package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	basePath     = "./data"
	metaFileName = "metadata.json"
)

type VersionMetadata struct {
	Version      string    `json:"version"`
	Size         int       `json:"size"`
	CreatedAt    time.Time `json:"created_at"`
	ContentType  string    `json:"content_type"`
	PasswordHash string    `json:"password_hash,omitempty"`
}

type Metadata struct {
	Key      string                      `json:"key"`
	Versions map[string]VersionMetadata `json:"versions"`
}

func hashPassword(pw string) string {
	h := sha256.Sum256([]byte(pw))
	return hex.EncodeToString(h[:])
}

func getExtensionFromContentType(contentType string) string {
	exts, _ := mime.ExtensionsByType(contentType)
	if len(exts) > 0 {
		return exts[0]
	}
	return ""
}

func writeMetadata(ns, key string, versionMeta VersionMetadata) {
	metaPath := filepath.Join(basePath, ns, metaFileName)
	var metas []Metadata
	_ = os.MkdirAll(filepath.Join(basePath, ns), os.ModePerm)

	if data, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(data, &metas)
	}

	found := false
	for i := range metas {
		if metas[i].Key == key {
			metas[i].Versions[versionMeta.Version] = versionMeta
			found = true
			break
		}
	}

	if !found {
		metas = append(metas, Metadata{
			Key:      key,
			Versions: map[string]VersionMetadata{versionMeta.Version: versionMeta},
		})
	}

	data, _ := json.MarshalIndent(metas, "", "  ")
	_ = os.WriteFile(metaPath, data, 0644)
}

func getLatestVersion(ns, key string) (*VersionMetadata, error) {
	metaPath := filepath.Join(basePath, ns, metaFileName)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}
	var metas []Metadata
	if err := json.Unmarshal(data, &metas); err != nil {
		return nil, err
	}
	for _, m := range metas {
		if m.Key == key {
			var latest VersionMetadata
			var latestTime int64
			for _, v := range m.Versions {
				if t := v.CreatedAt.UnixNano(); t > latestTime {
					latestTime = t
					latest = v
				}
			}
			return &latest, nil
		}
	}
	return nil, os.ErrNotExist
}

func getVersionMetadata(ns, key, version string) (*VersionMetadata, error) {
	metaPath := filepath.Join(basePath, ns, metaFileName)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}
	var metas []Metadata
	if err := json.Unmarshal(data, &metas); err != nil {
		return nil, err
	}
	for _, m := range metas {
		if m.Key == key {
			if vm, ok := m.Versions[version]; ok {
				return &vm, nil
			}
			break
		}
	}
	return nil, os.ErrNotExist
}

func uploadHandler(c *fiber.Ctx) error {
	ns := c.Params("namespace")
	key := c.Params("key")
	password := c.Get("X-Password", "")

	data := c.Body()
	contentType := http.DetectContentType(data)
	ext := getExtensionFromContentType(contentType)
	if ext == "" {
		ext = ".bin"
	}
	versionName := fmt.Sprintf("%s_%d%s", key, time.Now().UnixNano(), ext)
	versionPath := filepath.Join(basePath, ns, versionName)
	_ = os.MkdirAll(filepath.Join(basePath, ns), os.ModePerm)
	if err := os.WriteFile(versionPath, data, 0644); err != nil {
		return err
	}

	var pwHash string
	if password != "" {
		pwHash = hashPassword(password)
	}

	writeMetadata(ns, key, VersionMetadata{
		Version:      versionName,
		Size:         len(data),
		CreatedAt:    time.Now(),
		ContentType:  contentType,
		PasswordHash: pwHash,
	})

	return c.JSON(fiber.Map{
		"message": "Uploaded successfully",
		"key":     key,
		"version": versionName,
	})
}

func getHandler(c *fiber.Ctx) error {
	ns := c.Params("namespace")
	key := c.Params("key")
	password := c.Get("X-Password", "")

	meta, err := getLatestVersion(ns, key)
	if err != nil {
		return c.Status(404).SendString("Key not found")
	}

	if meta.PasswordHash != "" && hashPassword(password) != meta.PasswordHash {
		return c.Status(403).SendString("Unauthorized: password required")
	}

	filePath := filepath.Join(basePath, ns, meta.Version)
	c.Set("Content-Type", meta.ContentType)
	c.Set("X-Version", meta.Version)
	return c.SendFile(filePath)
}

func getVersionHandler(c *fiber.Ctx) error {
	ns := c.Params("namespace")
	version := c.Params("version")
	key := c.Query("key")
	password := c.Get("X-Password", "")

	meta, err := getVersionMetadata(ns, key, version)
	if err != nil {
		return c.Status(404).SendString("Version not found")
	}

	if meta.PasswordHash != "" && hashPassword(password) != meta.PasswordHash {
		return c.Status(403).SendString("Unauthorized: password required")
	}

	filePath := filepath.Join(basePath, ns, meta.Version)
	c.Set("Content-Type", meta.ContentType)
	c.Set("X-Version", meta.Version)
	return c.SendFile(filePath)
}

func listVersionsHandler(c *fiber.Ctx) error {
	ns := c.Params("namespace")
	key := c.Params("key")

	metaPath := filepath.Join(basePath, ns, metaFileName)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return c.Status(500).SendString("Error reading metadata")
	}

	var metas []Metadata
	if err := json.Unmarshal(data, &metas); err != nil {
		return c.Status(500).SendString("Metadata parse error")
	}

	for _, m := range metas {
		if m.Key == key {
			return c.JSON(m.Versions)
		}
	}
	return c.Status(404).SendString("Key not found")
}

func deleteHandler(c *fiber.Ctx) error {
	ns := c.Params("namespace")
	key := c.Params("key")
	password := c.Get("X-Password", "")

	metaPath := filepath.Join(basePath, ns, metaFileName)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return c.Status(500).SendString("Metadata file error")
	}

	var metas []Metadata
	if err := json.Unmarshal(data, &metas); err != nil {
		return c.Status(500).SendString("Metadata parse error")
	}

	updatedMetas := []Metadata{}
	keyFound := false

	for _, m := range metas {
		if m.Key == key {
			var latest VersionMetadata
			var latestTime int64
			for _, v := range m.Versions {
				if t := v.CreatedAt.UnixNano(); t > latestTime {
					latestTime = t
					latest = v
				}
			}
			if latest.PasswordHash != "" && hashPassword(password) != latest.PasswordHash {
				return c.Status(403).SendString("Unauthorized: incorrect password")
			}
			for _, v := range m.Versions {
				_ = os.Remove(filepath.Join(basePath, ns, v.Version))
			}
			keyFound = true
			continue
		}
		updatedMetas = append(updatedMetas, m)
	}

	if !keyFound {
		return c.Status(404).SendString("Key not found")
	}

	newData, _ := json.MarshalIndent(updatedMetas, "", "  ")
	_ = os.WriteFile(metaPath, newData, 0644)

	return c.JSON(fiber.Map{
		"message":  "Deleted key and all versions",
		"key":      key,
		"namespace": ns,
	})
}

func listNamespacesHandler(c *fiber.Ctx) error {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return c.Status(500).SendString("Failed to read namespaces")
	}
	namespaces := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			namespaces = append(namespaces, entry.Name())
		}
	}
	return c.JSON(fiber.Map{
		"namespaces": namespaces,
	})
}

func listKeysHandler(c *fiber.Ctx) error {
	ns := c.Params("namespace")
	metaPath := filepath.Join(basePath, ns, metaFileName)
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		return c.JSON(fiber.Map{
			"namespace": ns,
			"keys":      []string{},
		})
	}

data, err := os.ReadFile(metaPath)
	if err != nil {
		return c.Status(500).SendString("Error reading metadata")
	}

	var metas []Metadata
	if err := json.Unmarshal(data, &metas); err != nil {
		return c.Status(500).SendString("Metadata parse error")
	}

	keys := make([]string, len(metas))
	for i, m := range metas {
		keys[i] = m.Key
	}

	return c.JSON(fiber.Map{
		"namespace": ns,
		"keys":      keys,
	})
}

func main() {
	app := fiber.New()
	app.Use(compress.New())

	app.Post("/:namespace/:key", uploadHandler)
	app.Get("/:namespace/:key", getHandler)
	app.Get("/version/:namespace/:version", getVersionHandler)
	app.Get("/versions/:namespace/:key", listVersionsHandler)
	app.Get("/namespace", listNamespacesHandler)
	app.Get("/:namespace", listKeysHandler)
	app.Delete("/:namespace/:key", deleteHandler)

	port := 3000
	fmt.Printf("🚀 NebulaDB running on http://localhost:%d\n", port)
	app.Listen(":" + strconv.Itoa(port))
}
