// Package config liest die Konfiguration aus Umgebungsvariablen.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL string

	EmbeddingBaseURL     string
	EmbeddingModel       string
	EmbeddingDim         int
	EmbeddingAPIKey      string
	EmbeddingDocPrefix   string
	EmbeddingQueryPrefix string

	ChunkSize    int
	ChunkOverlap int
}

// LoadDotEnv liest eine .env-Datei und setzt nur Variablen, die noch nicht in der
// Umgebung gesetzt sind. Pfad: $KB_ENV_FILE, sonst <Verzeichnis des Binaries>/../.env.
// Eine fehlende Datei ist kein Fehler.
func LoadDotEnv() error {
	path := os.Getenv("KB_ENV_FILE")
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil
		}
		path = filepath.Join(filepath.Dir(exe), "..", ".env")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:          env("DATABASE_URL", "postgres://kb:kb@localhost:5433/knowledge_base?sslmode=disable"),
		EmbeddingBaseURL:     env("EMBEDDING_BASE_URL", "http://localhost:11434/v1"),
		EmbeddingModel:       env("EMBEDDING_MODEL", "nomic-embed-text"),
		EmbeddingAPIKey:      os.Getenv("EMBEDDING_API_KEY"),
		EmbeddingDocPrefix:   prefixEnv("EMBEDDING_DOC_PREFIX", "search_document: "),
		EmbeddingQueryPrefix: prefixEnv("EMBEDDING_QUERY_PREFIX", "search_query: "),
	}
	var err error
	if c.EmbeddingDim, err = envInt("EMBEDDING_DIM", 768); err != nil {
		return c, err
	}
	if c.ChunkSize, err = envInt("CHUNK_SIZE", 1200); err != nil {
		return c, err
	}
	if c.ChunkOverlap, err = envInt("CHUNK_OVERLAP", 150); err != nil {
		return c, err
	}
	if c.EmbeddingDim <= 0 || c.ChunkSize <= 0 || c.ChunkOverlap < 0 || c.ChunkOverlap >= c.ChunkSize {
		return c, fmt.Errorf("ungueltige Werte: EMBEDDING_DIM=%d CHUNK_SIZE=%d CHUNK_OVERLAP=%d",
			c.EmbeddingDim, c.ChunkSize, c.ChunkOverlap)
	}
	return c, nil
}

// prefixEnv liest ein Embedding-Praefix. Nicht gesetzt: nomic-embed-text erwartet
// "search_document: " bzw. "search_query: " (bessere Treffer), andere Modelle keins.
// Explizit leer gesetzt heisst: kein Praefix.
func prefixEnv(key, nomicDefault string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	if strings.HasPrefix(env("EMBEDDING_MODEL", "nomic-embed-text"), "nomic-embed-text") {
		return nomicDefault
	}
	return ""
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
