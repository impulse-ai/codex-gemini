package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	bundled "github.com/impulse-ai/codex-gemini/skills"
	"golang.org/x/term"
)

// MCPListEntry models an entry from `codex mcp list --json`.
type MCPListEntry struct {
	Name      string        `json:"name"`
	Enabled   bool          `json:"enabled"`
	Transport *MCPTransport `json:"transport"`
}

// MCPTransport represents the transport details of an MCP server registration.
type MCPTransport struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// setup handles `codex-gemini setup`.
func setup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: codex-gemini setup\nInstall the bundled Gemini skill, register this executable with Codex, and configure an API key.\nExisting custom files and registrations are preserved. No arguments are required.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("setup does not take positional arguments; received: %s", strings.Join(fs.Args(), " "))
	}

	// 1. Resolve Codex home & skill install path
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to determine user home directory: %w", err)
		}
		codexHome = filepath.Join(homeDir, ".codex")
	}
	skillTarget := filepath.Join(codexHome, "skills", "gemini")

	fmt.Fprintf(os.Stderr, "Installing skill to %s...\n", skillTarget)
	if err := installSkill(skillTarget); err != nil {
		return fmt.Errorf("skill installation failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Skill files verified/installed successfully.\n")

	// 2. Resolve executable path
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to determine executable path: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("failed to resolve absolute executable path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("failed to evaluate symlinks for executable path: %w", err)
	}

	// 3. Find codex CLI
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return fmt.Errorf("codex CLI not found in PATH: %w", err)
	}

	// 4. Register MCP
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fmt.Fprintf(os.Stderr, "Configuring Codex MCP registration...\n")
	if err := registerMCP(ctx, codexPath, exe); err != nil {
		return fmt.Errorf("MCP registration failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "MCP registration verified/completed successfully.\n")

	// 5. Authentication check / prompt
	fmt.Fprintf(os.Stderr, "Checking API key authentication...\n")
	key, err := loadKey()
	if err != nil || key == "" {
		// Key is missing or invalid
		isTerminal := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
		if !isTerminal {
			return errors.New("Gemini API key is not configured. Run 'codex-gemini auth' interactively or set GEMINI_API_KEY environment variable")
		}
		if authErr := auth(); authErr != nil {
			return fmt.Errorf("authentication failed: %w", authErr)
		}
	} else {
		fmt.Fprintf(os.Stderr, "API key is already configured.\n")
	}

	// 6. Reload notice
	fmt.Fprintf(os.Stderr, "\nSetup complete! If Codex is currently running, start a new Codex session or reload MCP connections and skills to load the gemini skill and MCP server.\n")
	return nil
}

// installSkill installs embedded skill files into targetDir (CODEX_HOME/skills/gemini).
func installSkill(targetDir string) error {
	// Embedded FS has files rooted under "gemini/"
	subFS, err := fs.Sub(bundled.FS, "gemini")
	if err != nil {
		return fmt.Errorf("failed to access embedded gemini skills filesystem: %w", err)
	}

	// Check if target directory is a symlink
	realTarget := targetDir
	linked := false
	if fi, err := os.Lstat(targetDir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			linked = true
			evalTarget, err := filepath.EvalSymlinks(targetDir)
			if err != nil {
				return fmt.Errorf("failed to resolve symlink %s: %w", targetDir, err)
			}
			realTarget = evalTarget
		}
	}

	// Collect all embedded files and preflight check existing files
	type fileEntry struct {
		relPath string
		content []byte
		mode    fs.FileMode
	}
	var files []fileEntry

	err = fs.WalkDir(subFS, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content, err := fs.ReadFile(subFS, path)
		if err != nil {
			return err
		}
		files = append(files, fileEntry{
			relPath: path,
			content: content,
			mode:    info.Mode().Perm(),
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed reading embedded skill files: %w", err)
	}

	// Preflight ALL files before any write
	for _, f := range files {
		destPath := filepath.Join(realTarget, f.relPath)
		destInfo, err := os.Lstat(destPath)
		if err == nil {
			if destInfo.IsDir() {
				return fmt.Errorf("conflicting directory exists at %s", destPath)
			}
			existingBytes, err := os.ReadFile(destPath)
			if err != nil {
				return fmt.Errorf("failed to read existing file for preflight %s: %w", destPath, err)
			}
			if !bytes.Equal(existingBytes, f.content) {
				return fmt.Errorf("conflicting skill file exists at %s with differing content; move the existing skill aside after reviewing it, then rerun setup", destPath)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("preflight check failed on %s: %w", destPath, err)
		} else if linked {
			return fmt.Errorf("linked skill is missing %s; update the source or move the symlink aside before setup", f.relPath)
		}
	}

	// Preflight passed: write missing files atomically
	for _, f := range files {
		destPath := filepath.Join(realTarget, f.relPath)
		if _, err := os.Stat(destPath); err == nil {
			// Already exists and identical from preflight
			continue
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for %s: %w", destPath, err)
		}

		tmpFile, err := os.CreateTemp(filepath.Dir(destPath), ".tmp-"+filepath.Base(destPath)+"-*")
		if err != nil {
			return fmt.Errorf("failed to create temporary file for %s: %w", destPath, err)
		}
		tmpName := tmpFile.Name()

		if _, err := tmpFile.Write(f.content); err != nil {
			tmpFile.Close()
			_ = os.Remove(tmpName)
			return fmt.Errorf("failed writing to temp file for %s: %w", destPath, err)
		}
		if err := tmpFile.Chmod(f.mode); err != nil {
			tmpFile.Close()
			_ = os.Remove(tmpName)
			return fmt.Errorf("failed setting permissions on %s: %w", destPath, err)
		}
		if err := tmpFile.Close(); err != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("failed closing temp file for %s: %w", destPath, err)
		}

		if err := os.Rename(tmpName, destPath); err != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("failed to atomically replace %s: %w", destPath, err)
		}
	}

	return nil
}

// registerMCP inspects existing MCP registrations via `codex mcp list --json` and registers
// gemini if missing or verifies matching configuration. Never echoes raw list output.
func registerMCP(ctx context.Context, codexPath, exe string) error {
	listCmd := exec.CommandContext(ctx, codexPath, "mcp", "list", "--json")
	var stdout bytes.Buffer
	listCmd.Stdout = &stdout
	listCmd.Stderr = io.Discard

	if err := listCmd.Run(); err != nil {
		return fmt.Errorf("failed running codex mcp list --json: %w; inspect Codex configuration directly", err)
	}

	var entries []MCPListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil || entries == nil {
		return errors.New("failed to parse JSON from 'codex mcp list --json'")
	}

	var geminiEntry *MCPListEntry
	for i := range entries {
		if entries[i].Name == "gemini" {
			geminiEntry = &entries[i]
			break
		}
	}

	if geminiEntry != nil {
		// Verify compatibility:
		// acceptable ONLY enabled stdio same executable and args first serve (preserve custom flags)
		if !geminiEntry.Enabled {
			return fmt.Errorf("existing MCP registration 'gemini' is disabled; inspect with 'codex mcp get gemini' and remove or re-enable it before continuing")
		}
		if geminiEntry.Transport == nil {
			return fmt.Errorf("existing MCP registration 'gemini' has no transport configured; inspect with 'codex mcp get gemini' and remove it if replacement is intended")
		}
		if geminiEntry.Transport.Type != "stdio" {
			return fmt.Errorf("existing MCP registration 'gemini' uses transport '%s' instead of 'stdio'; inspect with 'codex mcp get gemini' and remove it if replacement is intended", geminiEntry.Transport.Type)
		}

		// Check executable
		entryCmd := geminiEntry.Transport.Command
		entryCmdAbs, err := filepath.Abs(entryCmd)
		if err == nil {
			if eval, err := filepath.EvalSymlinks(entryCmdAbs); err == nil {
				entryCmdAbs = eval
			}
		} else {
			entryCmdAbs = entryCmd
		}

		if entryCmdAbs != exe {
			return fmt.Errorf("existing MCP registration 'gemini' points to executable %q (expected %q); inspect with 'codex mcp get gemini' and explicitly remove entry if replacement intended", entryCmd, exe)
		}

		if len(geminiEntry.Transport.Args) == 0 || geminiEntry.Transport.Args[0] != "serve" {
			return fmt.Errorf("existing MCP registration 'gemini' args do not start with 'serve'; inspect with 'codex mcp get gemini' and explicitly remove entry if replacement intended")
		}

		// Existing entry matches required executable and 'serve' command; preserve custom flags
		return nil
	}

	// Absent: run `codex mcp add gemini -- exe serve -concurrency 30 -rpm 60`
	addArgs := []string{"mcp", "add", "gemini", "--", exe, "serve", "-concurrency", "30", "-rpm", "60"}
	addCmd := exec.CommandContext(ctx, codexPath, addArgs...)
	addCmd.Stdout = io.Discard
	addCmd.Stderr = io.Discard

	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("failed executing codex mcp add gemini: %w; inspect Codex configuration directly", err)
	}

	return nil
}
