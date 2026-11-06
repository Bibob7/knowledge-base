// Package config reads the configuration from environment variables.
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

	// Similarity thresholds (cosine). They depend on the embedding model; see profiles.
	HookMinSim float64 // minimum similarity for the retrieval hook to inject an entry
	SimilarMin float64 // from here on an existing entry counts as "similar" to a new fact
}

// profile holds the model-specific defaults. The thresholds were measured on the
// real knowledge base (noise ceiling for off-topic prompts vs. matching entries).
type profile struct {
	dim                    int
	docPrefix, queryPrefix string
	hookMinSim, similarMin float64
}

// profiles are matched by model name prefix (longest first is not needed: keys are distinct).
var profiles = map[string]profile{
	// off-topic prompts <= 0.483, matching entries mostly > 0.5; related facts 0.74-0.77
	"bge-m3": {dim: 1024, hookMinSim: 0.52, similarMin: 0.70},
	// off-topic prompts <= 0.683, matching entries 0.67-0.77; contradicting facts ~0.94
	"nomic-embed-text": {dim: 768, docPrefix: "search_document: ", queryPrefix: "search_query: ", hookMinSim: 0.67, similarMin: 0.78},
	"qwen3-embedding":  {hookMinSim: 0.55, similarMin: 0.70},
	"embeddinggemma":   {dim: 768, docPrefix: "title: none | text: ", queryPrefix: "task: search result | query: ", hookMinSim: 0.65, similarMin: 0.78},
}

// unknownProfile is used for models without a measured profile. Calibrate it yourself
// (KB_HOOK_DEBUG=1 prints similarities) and override via KB_HOOK_MIN_SIM / KB_SIMILAR_MIN_SIM.
var unknownProfile = profile{hookMinSim: 0.60, similarMin: 0.75}

func profileFor(model string) profile {
	for name, p := range profiles {
		if strings.HasPrefix(model, name) {
			return p
		}
	}
	return unknownProfile
}

// FilePath is the user's configuration file: $XDG_CONFIG_HOME/knowledge-base/config.env,
// by default ~/.config/knowledge-base/config.env. It lives outside the plugin directories
// on purpose: those change with every plugin update or are deleted on uninstall, while the
// database (and its password) outlives both.
func FilePath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "knowledge-base", "config.env")
}

// envFilePath picks the configuration file: $KB_ENV_FILE, otherwise FilePath() if it exists,
// otherwise <binary directory>/../.env (installations from before the plugin).
func envFilePath() string {
	if p := os.Getenv("KB_ENV_FILE"); p != "" {
		return p
	}
	if p := FilePath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "..", ".env")
}

// LoadDotEnv reads the configuration file (see envFilePath) and only sets variables that
// are not already set in the environment. A missing file is not an error.
func LoadDotEnv() error {
	path := envFilePath()
	if path == "" {
		return nil
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
	model := env("EMBEDDING_MODEL", "bge-m3")
	prof := profileFor(model)
	c := Config{
		DatabaseURL:          env("DATABASE_URL", "postgres://kb:kb@localhost:5433/knowledge_base?sslmode=disable"),
		EmbeddingBaseURL:     env("EMBEDDING_BASE_URL", "http://localhost:11434/v1"),
		EmbeddingModel:       model,
		EmbeddingAPIKey:      os.Getenv("EMBEDDING_API_KEY"),
		EmbeddingDocPrefix:   prefixEnv("EMBEDDING_DOC_PREFIX", prof.docPrefix),
		EmbeddingQueryPrefix: prefixEnv("EMBEDDING_QUERY_PREFIX", prof.queryPrefix),
	}
	var err error
	if c.EmbeddingDim, err = envInt("EMBEDDING_DIM", prof.dim); err != nil {
		return c, err
	}
	if c.EmbeddingDim == 0 {
		return c, fmt.Errorf("EMBEDDING_DIM must be set for model %q (no known default)", model)
	}
	if c.ChunkSize, err = envInt("CHUNK_SIZE", 1200); err != nil {
		return c, err
	}
	if c.ChunkOverlap, err = envInt("CHUNK_OVERLAP", 150); err != nil {
		return c, err
	}
	if c.HookMinSim, err = envFloat("KB_HOOK_MIN_SIM", prof.hookMinSim); err != nil {
		return c, err
	}
	if c.SimilarMin, err = envFloat("KB_SIMILAR_MIN_SIM", prof.similarMin); err != nil {
		return c, err
	}
	if c.EmbeddingDim <= 0 || c.ChunkSize <= 0 || c.ChunkOverlap < 0 || c.ChunkOverlap >= c.ChunkSize {
		return c, fmt.Errorf("invalid values: EMBEDDING_DIM=%d CHUNK_SIZE=%d CHUNK_OVERLAP=%d",
			c.EmbeddingDim, c.ChunkSize, c.ChunkOverlap)
	}
	return c, nil
}

// prefixEnv reads an embedding prefix; if unset, the model profile's default applies.
// Explicitly set to empty means: no prefix.
func prefixEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
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

func envFloat(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}
