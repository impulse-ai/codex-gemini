package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const maxFile = 256 * 1024

type Files struct {
	root           *os.Root
	mu             sync.Mutex
	protectedRoots []string
}

func cleanPath(p string) (string, error) {
	p = filepath.Clean(p)
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("path must stay inside workspace")
	}
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		part = strings.ToLower(part) // Also protect case-insensitive macOS volumes.
		if part == ".git" || part == ".codex" || part == ".gemini-workers" || part == ".env" || strings.HasPrefix(part, ".env.") {
			return "", fmt.Errorf("protected path: %s", p)
		}
	}
	return p, nil
}

func (f *Files) validate(p string) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	if owns(f.protectedRoots, filepath.Join(f.root.Name(), p)) {
		return "", fmt.Errorf("service state is protected")
	}
	cur := ""
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := f.root.Lstat(cur)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlinks are not supported: %s", cur)
		}
	}
	return p, nil
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (f *Files) read(p string) ([]byte, error) {
	file, err := f.root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(file, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("file exceeds %d bytes", maxFile)
	}
	return b, nil
}

func owns(scopes []string, p string) bool {
	p = strings.ToLower(p)
	for _, s := range scopes {
		s = strings.ToLower(s) // Conservatively reserve case variants on every platform.
		if s == "." || p == s || strings.HasPrefix(p, s+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (f *Files) call(name string, args map[string]any, scopes []string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	str := func(k string) string { v, _ := args[k].(string); return v }
	p, err := f.validate(str("path"))
	if err != nil {
		return nil, err
	}
	switch name {
	case "list_files":
		var paths []string
		err := fs.WalkDir(f.root.FS(), p, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			_, e = cleanPath(path)
			if e != nil || owns(f.protectedRoots, filepath.Join(f.root.Name(), path)) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "vendor") {
				return fs.SkipDir
			}
			if !d.IsDir() {
				paths = append(paths, path)
			}
			if len(paths) >= 2000 {
				return fs.SkipAll
			}
			return nil
		})
		return map[string]any{"paths": paths, "limit": 2000}, err
	case "read_file":
		b, err := f.read(p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"content": string(b), "sha256": digest(b)}, nil
	case "write_file":
		if !owns(scopes, p) {
			return nil, fmt.Errorf("write denied: path is outside assigned write_paths")
		}
		content, ok := args["content"].(string)
		if !ok {
			return nil, fmt.Errorf("content must be a string")
		}
		if len(content) > maxFile {
			return nil, fmt.Errorf("content too large")
		}
		old, err := f.read(p)
		expected := str("expected_sha256")
		if os.IsNotExist(err) {
			if expected != "new" {
				return nil, fmt.Errorf("new files require expected_sha256=new")
			}
		} else if err != nil {
			return nil, err
		} else if expected != digest(old) {
			return nil, fmt.Errorf("file changed; read it again before writing")
		}
		if err = f.root.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return nil, err
		}
		// Replace via rename, so existing hard links are not modified in place.
		tmp := p + ".gemini-" + newID()
		mode := os.FileMode(0644)
		if st, e := f.root.Stat(p); e == nil {
			mode = st.Mode().Perm()
		}
		out, err := f.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return nil, err
		}
		defer f.root.Remove(tmp)
		_, err = out.WriteString(content)
		closeErr := out.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err = f.root.Rename(tmp, p); err != nil {
			return nil, err
		}
		return map[string]any{"path": p, "sha256": digest([]byte(content))}, nil
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}
