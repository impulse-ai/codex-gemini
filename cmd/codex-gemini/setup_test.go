package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bundled "github.com/impulse-ai/codex-gemini/skills"
)

func TestSetupHelpFlag(t *testing.T) {
	// setup -h should return nil with no effects
	err := setup([]string{"-h"})
	if err != nil {
		t.Fatalf("expected setup -h to return nil, got: %v", err)
	}

	err = setup([]string{"--help"})
	if err != nil {
		t.Fatalf("expected setup --help to return nil, got: %v", err)
	}
}

func TestInstallSkill_NewSkillContentMatchesBundled(t *testing.T) {
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "skills", "gemini")

	if err := installSkill(targetDir); err != nil {
		t.Fatalf("installSkill failed: %v", err)
	}

	expectedSkill, err := bundled.FS.ReadFile("gemini/SKILL.md")
	if err != nil {
		t.Fatalf("failed to read embedded gemini/SKILL.md: %v", err)
	}

	installedSkill, err := os.ReadFile(filepath.Join(targetDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("failed to read installed SKILL.md: %v", err)
	}

	if string(installedSkill) != string(expectedSkill) {
		t.Fatalf("installed SKILL.md does not match bundled content")
	}
}

func TestInstallSkill_RepeatNoRewriteModTime(t *testing.T) {
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "skills", "gemini")

	if err := installSkill(targetDir); err != nil {
		t.Fatalf("first installSkill failed: %v", err)
	}

	skillPath := filepath.Join(targetDir, "SKILL.md")
	// Set past modtime to detect any rewrite/mutation
	pastTime := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(skillPath, pastTime, pastTime); err != nil {
		t.Fatalf("failed to set modtime: %v", err)
	}

	fiBefore, err := os.Stat(skillPath)
	if err != nil {
		t.Fatalf("failed to stat skillPath: %v", err)
	}

	// Repeat install
	if err := installSkill(targetDir); err != nil {
		t.Fatalf("second installSkill failed: %v", err)
	}

	fiAfter, err := os.Stat(skillPath)
	if err != nil {
		t.Fatalf("failed to stat skillPath after rerun: %v", err)
	}

	if !fiBefore.ModTime().Equal(fiAfter.ModTime()) {
		t.Fatalf("expected modtime unchanged on repeat install (%v), got (%v)", fiBefore.ModTime(), fiAfter.ModTime())
	}
}

func TestInstallSkill_PreserveDifferingCustomFile(t *testing.T) {
	tempDir := t.TempDir()
	targetDir := filepath.Join(tempDir, "skills", "gemini")

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	skillPath := filepath.Join(targetDir, "SKILL.md")
	customContent := []byte("custom skill instructions modified by user")
	if err := os.WriteFile(skillPath, customContent, 0644); err != nil {
		t.Fatalf("failed to write custom skill: %v", err)
	}

	err := installSkill(targetDir)
	if err == nil {
		t.Fatalf("expected installSkill to fail when differing file exists, but got nil")
	}

	// Ensure the differing custom file content was preserved untouched
	contentAfter, readErr := os.ReadFile(skillPath)
	if readErr != nil {
		t.Fatalf("failed to read skill file after install attempt: %v", readErr)
	}
	if string(contentAfter) != string(customContent) {
		t.Fatalf("custom skill file was modified; expected %q, got %q", string(customContent), string(contentAfter))
	}
}

func TestInstallSkill_AcceptIdenticalSymlinkWithoutMutation(t *testing.T) {
	tempDir := t.TempDir()
	actualDir := filepath.Join(tempDir, "external-gemini")
	symlinkTarget := filepath.Join(tempDir, "skills", "gemini")

	// First install to actualDir so it has identical embedded contents
	if err := installSkill(actualDir); err != nil {
		t.Fatalf("failed to setup initial skill in actualDir: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(symlinkTarget), 0755); err != nil {
		t.Fatalf("failed to create parent dir: %v", err)
	}

	if err := os.Symlink(actualDir, symlinkTarget); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	skillFile := filepath.Join(actualDir, "SKILL.md")
	pastTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(skillFile, pastTime, pastTime); err != nil {
		t.Fatalf("failed to set modtime: %v", err)
	}

	fiBefore, err := os.Stat(skillFile)
	if err != nil {
		t.Fatalf("failed to stat skillFile: %v", err)
	}

	// installSkill through symlink
	if err := installSkill(symlinkTarget); err != nil {
		t.Fatalf("expected installSkill to succeed on identical symlink, got: %v", err)
	}

	fiAfter, err := os.Stat(skillFile)
	if err != nil {
		t.Fatalf("failed to stat skillFile after rerun: %v", err)
	}

	if !fiBefore.ModTime().Equal(fiAfter.ModTime()) {
		t.Fatalf("expected symlinked skill file modtime to remain unchanged, before: %v, after: %v", fiBefore.ModTime(), fiAfter.ModTime())
	}
}

func TestInstallSkill_RejectMissingSkillUnderExternalSymlinkWithoutWritingTarget(t *testing.T) {
	tempDir := t.TempDir()
	externalDir := filepath.Join(tempDir, "external-gemini")
	if err := os.MkdirAll(externalDir, 0755); err != nil {
		t.Fatalf("failed to create external dir: %v", err)
	}

	symlinkTarget := filepath.Join(tempDir, "skills", "gemini")
	if err := os.MkdirAll(filepath.Dir(symlinkTarget), 0755); err != nil {
		t.Fatalf("failed to create parent dir: %v", err)
	}
	if err := os.Symlink(externalDir, symlinkTarget); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// External dir is empty, missing skill files
	err := installSkill(symlinkTarget)
	if err == nil {
		t.Fatalf("expected installSkill to reject missing skill under external symlink, got nil")
	}

	// Verify target was not written
	destSkillPath := filepath.Join(externalDir, "SKILL.md")
	if _, statErr := os.Stat(destSkillPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected %s not to be written, stat err: %v", destSkillPath, statErr)
	}
}

// helper to create fake codex executable script
func createFakeCodex(t *testing.T, scriptBody string) (string, string) {
	t.Helper()
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "codex_invocations.log")
	binPath := filepath.Join(tempDir, "codex")

	content := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
%s
`, logPath, scriptBody)

	if err := os.WriteFile(binPath, []byte(content), 0755); err != nil {
		t.Fatalf("failed to create fake codex script: %v", err)
	}

	return binPath, logPath
}

func TestRegisterMCP_Absent_AddsAbsoluteExeNoWorkspaceFlags(t *testing.T) {
	// mcp list --json returns empty list []
	script := `if [ "$1" = "mcp" ] && [ "$2" = "list" ]; then
  echo "[]"
  exit 0
fi
if [ "$1" = "mcp" ] && [ "$2" = "add" ]; then
  exit 0
fi
exit 0`

	codexPath, logPath := createFakeCodex(t, script)
	exePath := "/opt/bin/codex-gemini"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := registerMCP(ctx, codexPath, exePath)
	if err != nil {
		t.Fatalf("registerMCP failed: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	logStr := string(logBytes)

	// Verify codex mcp add was called with absolute exe and no workspace flags
	expectedAdd := fmt.Sprintf("mcp add gemini -- %s serve -concurrency 30 -rpm 60", exePath)
	if !strings.Contains(logStr, expectedAdd) {
		t.Fatalf("expected add command %q in invocations:\n%s", expectedAdd, logStr)
	}
}

func TestRegisterMCP_MatchingExistingCustomFlags_UnchangedNoAdd(t *testing.T) {
	exePath := "/opt/bin/codex-gemini"
	// Existing registration with custom flags: serve -custom-flag 123
	fixture := fmt.Sprintf(`[
  {
    "name": "gemini",
    "enabled": true,
    "transport": {
      "type": "stdio",
      "command": %q,
      "args": ["serve", "-custom-flag", "123"]
    }
  }
]`, exePath)

	script := fmt.Sprintf(`if [ "$1" = "mcp" ] && [ "$2" = "list" ]; then
  cat << 'EOF'
%s
EOF
  exit 0
fi
exit 0`, fixture)

	codexPath, logPath := createFakeCodex(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := registerMCP(ctx, codexPath, exePath)
	if err != nil {
		t.Fatalf("registerMCP failed on matching config: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	logStr := string(logBytes)

	if strings.Contains(logStr, "mcp add") {
		t.Fatalf("expected no 'mcp add' call when matching registration exists, got invocations:\n%s", logStr)
	}
}

func TestRegisterMCP_Disabled_ErrorsNoAdd(t *testing.T) {
	exePath := "/opt/bin/codex-gemini"
	fixture := fmt.Sprintf(`[
  {
    "name": "gemini",
    "enabled": false,
    "transport": {
      "type": "stdio",
      "command": %q,
      "args": ["serve"]
    }
  }
]`, exePath)

	script := fmt.Sprintf(`if [ "$1" = "mcp" ] && [ "$2" = "list" ]; then
  cat << 'EOF'
%s
EOF
  exit 0
fi
exit 0`, fixture)

	codexPath, logPath := createFakeCodex(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := registerMCP(ctx, codexPath, exePath)
	if err == nil {
		t.Fatalf("expected error when registration is disabled, got nil")
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	logStr := string(logBytes)

	if strings.Contains(logStr, "mcp add") {
		t.Fatalf("expected no 'mcp add' when registration is disabled, got invocations:\n%s", logStr)
	}
}

func TestRegisterMCP_MismatchedBinary_ErrorsNoAdd(t *testing.T) {
	fixture := `[
  {
    "name": "gemini",
    "enabled": true,
    "transport": {
      "type": "stdio",
      "command": "/other/bin/codex-gemini",
      "args": ["serve"]
    }
  }
]`

	script := fmt.Sprintf(`if [ "$1" = "mcp" ] && [ "$2" = "list" ]; then
  cat << 'EOF'
%s
EOF
  exit 0
fi
exit 0`, fixture)

	codexPath, logPath := createFakeCodex(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := registerMCP(ctx, codexPath, "/opt/bin/codex-gemini")
	if err == nil {
		t.Fatalf("expected error when binary is mismatched, got nil")
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	logStr := string(logBytes)

	if strings.Contains(logStr, "mcp add") {
		t.Fatalf("expected no 'mcp add' when binary mismatches, got invocations:\n%s", logStr)
	}
}

func TestRegisterMCP_MalformedJSON_NoAddNoSecretExposed(t *testing.T) {
	secretPayload := "SECRET_AUTH_TOKEN_MALFORMED_12345"
	script := fmt.Sprintf(`if [ "$1" = "mcp" ] && [ "$2" = "list" ]; then
  echo "{ malformed json: %s }"
  exit 0
fi
exit 0`, secretPayload)

	codexPath, logPath := createFakeCodex(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := registerMCP(ctx, codexPath, "/opt/bin/codex-gemini")
	if err == nil {
		t.Fatalf("expected error on malformed JSON, got nil")
	}

	if strings.Contains(err.Error(), secretPayload) {
		t.Fatalf("error message exposed fixture secret: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	logStr := string(logBytes)

	if strings.Contains(logStr, "mcp add") {
		t.Fatalf("expected no 'mcp add' on malformed JSON, got invocations:\n%s", logStr)
	}
}

func TestRegisterMCP_CLIError_NoAddNoSecretExposed(t *testing.T) {
	secretStderr := "SECRET_TOKEN_IN_STDERR_54321"
	script := fmt.Sprintf(`if [ "$1" = "mcp" ] && [ "$2" = "list" ]; then
  echo "%s" >&2
  exit 1
fi
exit 0`, secretStderr)

	codexPath, logPath := createFakeCodex(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := registerMCP(ctx, codexPath, "/opt/bin/codex-gemini")
	if err == nil {
		t.Fatalf("expected error on CLI error, got nil")
	}

	if strings.Contains(err.Error(), secretStderr) {
		t.Fatalf("error message exposed fixture secret from stderr: %v", err)
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	logStr := string(logBytes)

	if strings.Contains(logStr, "mcp add") {
		t.Fatalf("expected no 'mcp add' on CLI error, got invocations:\n%s", logStr)
	}
}
