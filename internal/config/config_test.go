package config

import (
	"os"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"EMBEDDING_MODEL", "EMBEDDING_DIM", "EMBEDDING_DOC_PREFIX", "EMBEDDING_QUERY_PREFIX",
		"KB_HOOK_MIN_SIM", "KB_SIMILAR_MIN_SIM", "CHUNK_SIZE", "CHUNK_OVERLAP"} {
		// Unset (not empty): an empty prefix variable means "explicitly no prefix".
		if old, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { os.Setenv(k, old) })
			os.Unsetenv(k)
		}
	}
}

func TestDefaultsAreBgeM3(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.EmbeddingModel != "bge-m3" || c.EmbeddingDim != 1024 || c.EmbeddingDocPrefix != "" || c.EmbeddingQueryPrefix != "" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.HookMinSim != 0.52 || c.SimilarMin != 0.70 {
		t.Errorf("thresholds: hook %v similar %v", c.HookMinSim, c.SimilarMin)
	}
}

func TestNomicProfile(t *testing.T) {
	clearEnv(t)
	t.Setenv("EMBEDDING_MODEL", "nomic-embed-text:latest")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.EmbeddingDim != 768 || c.EmbeddingDocPrefix != "search_document: " || c.HookMinSim != 0.67 || c.SimilarMin != 0.78 {
		t.Errorf("unexpected nomic profile: %+v", c)
	}
}

func TestOverridesAndUnknownModel(t *testing.T) {
	clearEnv(t)
	t.Setenv("EMBEDDING_MODEL", "some-new-model")
	if _, err := Load(); err == nil {
		t.Fatal("an unknown model without EMBEDDING_DIM must be an error")
	}
	t.Setenv("EMBEDDING_DIM", "512")
	t.Setenv("KB_HOOK_MIN_SIM", "0.4")
	t.Setenv("EMBEDDING_QUERY_PREFIX", "q: ")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.EmbeddingDim != 512 || c.HookMinSim != 0.4 || c.SimilarMin != 0.75 || c.EmbeddingQueryPrefix != "q: " {
		t.Errorf("unexpected config: %+v", c)
	}
}
