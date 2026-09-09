// Package memory stores shared retrieval knowledge without replaying transcripts.
package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	_ "modernc.org/sqlite"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type Embedder interface {
	Embed(context.Context, []string, string) ([][]float32, error)
}
type Store struct {
	mu       sync.Mutex
	db       *sql.DB
	embedder Embedder
	model    string
}
type Hit struct {
	Source    string  `json:"source"`
	Workspace string  `json:"workspace"`
	Title     string  `json:"title"`
	Chunk     int     `json:"chunk"`
	Text      string  `json:"text"`
	Score     float64 `json:"score"`
}
type Result struct {
	Hits      []Hit  `json:"hits"`
	Mode      string `json:"mode"`
	Warning   string `json:"warning,omitempty"`
	Pending   int    `json:"pending_embeddings"`
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"excerpt_bytes"`
}
type Stats struct {
	Chunks            int    `json:"chunks"`
	CachedVectors     int    `json:"cached_vectors"`
	EmbeddingRequests int    `json:"embedding_requests"`
	EmbeddedBytes     int    `json:"embedded_bytes"`
	Model             string `json:"embedding_model"`
}

func hash(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func Open(path, model string, e Embedder) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS chunks(workspace TEXT NOT NULL,source TEXT NOT NULL,title TEXT NOT NULL,part INTEGER NOT NULL,text TEXT NOT NULL,hash TEXT NOT NULL,PRIMARY KEY(workspace,source,part));
 CREATE INDEX IF NOT EXISTS chunks_scope ON chunks(workspace);
 CREATE TABLE IF NOT EXISTS vectors(model TEXT NOT NULL,task TEXT NOT NULL,hash TEXT NOT NULL,vector BLOB NOT NULL,PRIMARY KEY(model,task,hash));
 CREATE TABLE IF NOT EXISTS counters(name TEXT PRIMARY KEY,value INTEGER NOT NULL);
 PRAGMA user_version=1;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, model: model, embedder: e}, nil
}
func (s *Store) Close() error { return s.db.Close() }

// Upsert indexes bounded explicit context, not arbitrary repository or private model text.
func (s *Store) Upsert(workspace, source, title, text string) error {
	if len(text) > 32768 || len(title) > 200 || source == "" {
		return fmt.Errorf("memory document exceeds limits or lacks source")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM chunks WHERE workspace=? AND source=?`, workspace, source); err != nil {
		return err
	}
	part := 0
	for len(text) > 0 {
		n := min(1800, len(text))
		for n > 0 && !utf8.ValidString(text[:n]) {
			n--
		}
		if n == 0 {
			return fmt.Errorf("memory text must be UTF-8")
		}
		chunk := text[:n]
		text = text[n:]
		_, err = tx.Exec(`INSERT INTO chunks VALUES(?,?,?,?,?,?)`, workspace, source, title, part, chunk, hash(chunk))
		if err != nil {
			return err
		}
		part++
	}
	return tx.Commit()
}
func normalized(v []float32) ([]float32, error) {
	if len(v) != 768 {
		return nil, fmt.Errorf("expected 768 embedding dimensions, got %d", len(v))
	}
	sum := 0.0
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, fmt.Errorf("invalid embedding")
		}
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / math.Sqrt(sum))
	}
	return out, nil
}
func (s *Store) cached(ctx context.Context, text, task string) ([]float32, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT vector FROM vectors WHERE model=? AND task=? AND hash=?`, s.model, task, hash(text)).Scan(&b)
	if err == nil {
		var v []float32
		err = json.Unmarshal(b, &v)
		return v, err
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	return nil, nil
}
func (s *Store) embed(ctx context.Context, texts []string, task string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if s.embedder == nil {
		return nil, fmt.Errorf("semantic embeddings disabled")
	}
	// Record attempted calls and input bytes, including failures; API billing token
	// counts are unavailable for Developer API embeddings and are never fabricated.
	bytes := 0
	for _, v := range texts {
		bytes += len(v)
	}
	for k, v := range map[string]int{"requests": 1, "bytes": bytes} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO counters VALUES(?,?) ON CONFLICT(name) DO UPDATE SET value=value+excluded.value`, k, v); err != nil {
			return nil, err
		}
	}
	vectors, err := s.embedder.Embed(ctx, texts, task)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("embedding count mismatch")
	}
	for i, v := range vectors {
		v, err = normalized(v)
		if err != nil {
			return nil, err
		}
		vectors[i] = v
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i, v := range vectors {
		b, _ := json.Marshal(v)
		if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO vectors VALUES(?,?,?,?)`, s.model, task, hash(texts[i]), b); err != nil {
			return nil, err
		}
	}
	return vectors, tx.Commit()
}
func words(text string) map[string]bool {
	out := map[string]bool{}
	stop := map[string]bool{"the": true, "and": true, "for": true, "with": true, "from": true, "that": true, "this": true, "how": true, "what": true, "which": true, "are": true, "was": true, "were": true, "does": true, "can": true, "do": true, "we": true, "to": true, "of": true, "in": true, "on": true, "is": true, "it": true, "an": true}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) > 1 && !stop[w] {
			out[w] = true
		}
	}
	return out
}
func (s *Store) Search(ctx context.Context, workspace, query string, semantic bool, limit, maxBytes int) (Result, error) {
	out := Result{Hits: []Hit{}, Mode: "keyword"}
	if workspace == "" || strings.TrimSpace(query) == "" || len(query) > 2000 {
		return out, fmt.Errorf("workspace and query (1–2000 bytes) required")
	}
	if limit == 0 {
		limit = 4
	}
	if maxBytes == 0 {
		maxBytes = 6000
	}
	if limit < 1 || limit > 8 || maxBytes < 256 || maxBytes > 12000 {
		return out, fmt.Errorf("limit must be 1–8 and max_bytes 256–12000")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bound scan/memory for the local single-user service. Expose truncation rather
	// than claiming an exhaustive global nearest-neighbor search.
	rows, err := s.db.QueryContext(ctx, `SELECT source,workspace,title,part,text FROM chunks WHERE workspace=? ORDER BY rowid DESC LIMIT 5001`, workspace)
	if err != nil {
		return out, err
	}
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err = rows.Scan(&h.Source, &h.Workspace, &h.Title, &h.Chunk, &h.Text); err != nil {
			rows.Close()
			return out, err
		}
		hits = append(hits, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(hits) > 5000 {
		out.Truncated = true
		hits = hits[:5000]
	}
	if len(hits) == 0 {
		return out, nil
	}
	var qv []float32
	if semantic && s.embedder != nil {
		pending := []string{}
		seen := map[string]bool{}
		for _, h := range hits {
			v, e := s.cached(ctx, h.Text, "RETRIEVAL_DOCUMENT")
			if e != nil {
				return out, e
			}
			if v == nil && !seen[hash(h.Text)] {
				seen[hash(h.Text)] = true
				pending = append(pending, h.Text)
			}
		}
		// At most 16 small document chunks and one query per search; repeated searches
		// reuse durable vectors. Never embed the whole archive during startup.
		out.Pending = max(0, len(pending)-16)
		_, e := s.embed(ctx, pending[:min(16, len(pending))], "RETRIEVAL_DOCUMENT")
		if e == nil {
			qv, e = s.cached(ctx, query, "RETRIEVAL_QUERY")
			if e == nil && qv == nil {
				var vs [][]float32
				vs, e = s.embed(ctx, []string{query}, "RETRIEVAL_QUERY")
				if e == nil {
					qv = vs[0]
				}
			}
		}
		if e != nil {
			out.Warning = "Semantic retrieval unavailable; used local keyword matches."
			out.Pending = len(pending)
			qv = nil
		} else {
			out.Mode = "hybrid"
		}
	} else if semantic {
		out.Warning = "Embeddings disabled; used local keyword matches."
	}
	qw := words(query)
	for i := range hits {
		hw := words(hits[i].Title + " " + hits[i].Text)
		matched := 0
		for w := range qw {
			if hw[w] {
				matched++
			}
		}
		if len(qw) > 0 {
			hits[i].Score = float64(matched) / float64(len(qw))
		}
		if qv != nil {
			v, e := s.cached(ctx, hits[i].Text, "RETRIEVAL_DOCUMENT")
			if e != nil {
				return out, e
			}
			if len(v) == len(qv) {
				dot := 0.0
				for j, x := range v {
					dot += float64(x) * float64(qv[j])
				}
				if dot > 0.25 {
					hits[i].Score += dot
				}
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	seen := map[string]bool{}
	for _, h := range hits {
		if h.Score <= 0 || (qv != nil && h.Score < hits[0].Score*0.65) || seen[hash(h.Text)] {
			continue
		}
		seen[hash(h.Text)] = true
		if len(out.Hits) >= limit {
			out.Truncated = true
			break
		}
		available := maxBytes - out.Bytes
		if available <= 0 {
			out.Truncated = true
			break
		}
		if len(h.Text) > available {
			n := available
			for n > 0 && !utf8.ValidString(h.Text[:n]) {
				n--
			}
			h.Text = h.Text[:n]
			out.Truncated = true
		}
		out.Bytes += len(h.Text)
		out.Hits = append(out.Hits, h)
	}
	return out, nil
}
func (s *Store) Stats() (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Stats{Model: s.model}
	for _, q := range []struct {
		sql  string
		dest *int
	}{{`SELECT count(*) FROM chunks`, &out.Chunks}, {`SELECT count(*) FROM vectors`, &out.CachedVectors}, {`SELECT coalesce(sum(value),0) FROM counters WHERE name='requests'`, &out.EmbeddingRequests}, {`SELECT coalesce(sum(value),0) FROM counters WHERE name='bytes'`, &out.EmbeddedBytes}} {
		if err := s.db.QueryRow(q.sql).Scan(q.dest); err != nil {
			return out, err
		}
	}
	return out, nil
}
