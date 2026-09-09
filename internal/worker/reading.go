package worker

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func integerArg(args map[string]any, key string, def int) (int, error) {
	v, ok := args[key]
	if !ok {
		return def, nil
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case float64:
		if n >= 1 && n <= 10000000 && n == float64(int(n)) {
			return int(n), nil
		}
	}
	return 0, fmt.Errorf("%s must be a positive integer", key)
}
func readPage(b []byte, args map[string]any) (map[string]any, error) {
	start, err := integerArg(args, "start_line", 1)
	if err != nil {
		return nil, err
	}
	count, err := integerArg(args, "max_lines", 200)
	if err != nil {
		return nil, err
	}
	if start < 1 || count < 1 || count > 400 {
		return nil, fmt.Errorf("start_line must be positive and max_lines must be 1–400")
	}
	lines := strings.SplitAfter(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if start > len(lines) && !(len(lines) == 0 && start == 1) {
		return nil, fmt.Errorf("start_line is past the end of the file (%d lines)", len(lines))
	}
	end := start - 1
	var content strings.Builder
	for end < len(lines) && end < start-1+count {
		line := lines[end]
		if content.Len()+len(line) > 8192 {
			if content.Len() == 0 {
				return nil, fmt.Errorf("line %d exceeds 8192 bytes; use search_files for targeted snippets", end+1)
			}
			break
		}
		content.WriteString(line)
		end++
	}
	next := 0
	if end < len(lines) {
		next = end + 1
	}
	return map[string]any{"content": content.String(), "sha256": digest(b), "start_line": start, "end_line": end, "total_lines": len(lines), "next_line": next, "complete": start == 1 && next == 0}, nil
}
func (f *Files) search(path, query string) (any, error) {
	if strings.TrimSpace(query) == "" || len(query) > 200 {
		return nil, fmt.Errorf("query must contain 1–200 bytes")
	}
	type match struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Snippet string `json:"snippet"`
	}
	matches := []match{}
	scanned, visited, bytes, skipped := 0, 0, 0, 0
	truncated := false
	err := fs.WalkDir(f.root.FS(), path, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			if p == path {
				return e
			}
			skipped++
			return nil
		}
		visited++
		if visited > 2000 || scanned >= 500 || bytes >= 4*1024*1024 {
			truncated = true
			return fs.SkipAll
		}
		if _, e = cleanPath(p); e != nil || owns(f.protectedRoots, filepath.Join(f.root.Name(), p)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "vendor" || d.Name() == ".next" {
				return fs.SkipDir
			}
			return nil
		}
		scanned++
		b, e := f.read(p)
		if e != nil {
			skipped++
			return nil
		}
		bytes += len(b)
		if strings.IndexByte(string(b), 0) >= 0 {
			skipped++
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			at := strings.Index(line, query)
			if at < 0 {
				continue
			}
			start := max(0, at-80)
			end := min(len(line), at+len(query)+120)
			matches = append(matches, match{Path: p, Line: i + 1, Snippet: strings.ToValidUTF8(line[start:end], "")})
			if len(matches) >= 20 {
				truncated = true
				return fs.SkipAll
			}
		}
		return nil
	})
	return map[string]any{"matches": matches, "truncated": truncated, "files_scanned": scanned, "skipped_files": skipped}, err
}
