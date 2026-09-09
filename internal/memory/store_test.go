package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeEmbedder struct {
	calls atomic.Int32
	fail  bool
}

func (e *fakeEmbedder) Embed(ctx context.Context, texts []string, task string) ([][]float32, error) {
	e.calls.Add(1)
	if e.fail {
		return nil, fmt.Errorf("quota")
	}
	vs := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, 768)
		if strings.Contains(text, "stop") || strings.Contains(text, "terminate") {
			v[0] = 1
		} else {
			v[1] = 1
		}
		vs[i] = v
	}
	return vs, nil
}
func TestSharedCacheScopeAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	e := &fakeEmbedder{}
	s, err := Open(path, "test", e)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct{ w, source, text string }{{"/a", "ctx-a", "terminate requests before releasing control"}, {"/b", "ctx-b", "secret workspace stop information"}} {
		if err = s.Upsert(d.w, d.source, "brief", d.text); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Search(ctx, "/a", "stop operations", true, 4, 1000)
			if err != nil || len(r.Hits) != 1 || r.Hits[0].Source != "ctx-a" {
				t.Errorf("scope or semantic retrieval: %+v %v", r, err)
			}
		}()
	}
	wg.Wait()
	if e.calls.Load() != 2 {
		t.Fatalf("identical searches repeated inference: %d", e.calls.Load())
	}
	s.Close()
	s, err = Open(path, "test", e)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Search(ctx, "/a", "stop operations", true, 4, 1000); err != nil {
		t.Fatal(err)
	}
	if e.calls.Load() != 2 {
		t.Fatal("cache not durable")
	}
	st, err := s.Stats()
	if err != nil || st.EmbeddingRequests != 2 || st.EmbeddedBytes == 0 {
		t.Fatalf("bad cost counters %+v %v", st, err)
	}
}
func TestLocalFallbackAndBoundedEmbedding(t *testing.T) {
	e := &fakeEmbedder{}
	s, err := Open(filepath.Join(t.TempDir(), "memory.sqlite"), "test", e)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := range 18 {
		if err = s.Upsert("/a", fmt.Sprint(i), "brief", fmt.Sprintf("terminate item %d %s", i, strings.Repeat("details ", 100))); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Search(ctx, "/a", "terminate", false, 2, 256)
	if err != nil || r.Bytes > 256 || len(r.Hits) > 2 || !r.Truncated || e.calls.Load() != 0 {
		t.Fatalf("unbounded local retrieval %+v %v", r, err)
	}
	r, err = s.Search(ctx, "/a", "stop", true, 4, 1000)
	if err != nil || r.Pending != 2 || e.calls.Load() != 2 {
		t.Fatalf("unbounded indexing %+v %v", r, err)
	}
	r, err = s.Search(ctx, "/a", "stop", true, 4, 1000)
	if err != nil || r.Pending != 0 || e.calls.Load() != 3 {
		t.Fatalf("failed incremental cached indexing %+v %v", r, err)
	}
	e.fail = true
	r, err = s.Search(ctx, "/a", "terminate unknown", true, 4, 1000)
	if err != nil || r.Mode != "keyword" || r.Warning == "" || len(r.Hits) == 0 {
		t.Fatalf("failed local fallback %+v %v", r, err)
	}
}
func TestMemoryValidationAndDedup(t *testing.T) {
	e := &fakeEmbedder{}
	s, err := Open(filepath.Join(t.TempDir(), "memory.sqlite"), "test", e)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"a", "b"} {
		if err = s.Upsert("/a", id, "brief", "terminate requests"); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Search(context.Background(), "/a", "stop", true, 4, 1000)
	if err != nil || len(r.Hits) != 1 {
		t.Fatalf("duplicates replayed %+v %v", r, err)
	}
	st, _ := s.Stats()
	if st.CachedVectors != 2 {
		t.Fatalf("duplicate vectors %+v", st)
	}
	for _, q := range []string{"", strings.Repeat("x", 2001)} {
		if _, err = s.Search(context.Background(), "/a", q, true, 4, 1000); err == nil {
			t.Fatal("unbounded query accepted")
		}
	}
	if _, err = s.Search(context.Background(), "/a", "q", false, 9, 1000); err == nil {
		t.Fatal("unbounded top k")
	}
	if _, err = normalized([]float32{1}); err == nil {
		t.Fatal("mismatched vector accepted")
	}
}
