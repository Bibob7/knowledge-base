package viz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kevin/knowledge-base/internal/store"
)

type fakeSource struct{}

func (fakeSource) Snapshot(ctx context.Context, limit int) (*store.Snapshot, error) {
	now := time.Now()
	return &store.Snapshot{Total: 2, Model: "m", Docs: []store.SnapshotDoc{
		{ID: 2, Title: "b", Project: "p", Kind: "fact", Status: "active", CreatedAt: now, UpdatedAt: now, Content: "<script>x</script>", Vec: []float32{1, 0}},
		{ID: 1, Title: "a", Kind: "fact", Status: "superseded", CreatedAt: now, UpdatedAt: now, Content: "a", Vec: []float32{0, 1}},
	}}, nil
}

func TestHandler(t *testing.T) {
	h := Handler(Options{Source: fakeSource{}})

	req := httptest.NewRequest("GET", "http://localhost:8765/api/data", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var d dataJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Shown != 2 || d.Total != 2 || len(d.Projects) != 1 || d.Projects[0] != "p" {
		t.Fatalf("unexpected data: %+v", d)
	}
	if d.Docs[1].Tags == nil {
		t.Fatal("tags must be [] not null")
	}

	// DNS rebinding: a foreign Host header must be rejected.
	req = httptest.NewRequest("GET", "http://evil.example/api/data", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign host: status %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "http://127.0.0.1:1/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.Len() == 0 {
		t.Fatalf("index: status %d", rec.Code)
	}
}

func TestServeRefusesNonLoopback(t *testing.T) {
	err := Serve(context.Background(), "0.0.0.0:0", Options{Source: fakeSource{}}, nil)
	if err == nil {
		t.Fatal("expected refusal for a non-loopback address")
	}
}
