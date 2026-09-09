// Package service hosts a multi-client MCP server on a private local Unix socket.
package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/impulse-ai/codex-gemini/internal/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func SocketPath(state string) (string, error) {
	abs, err := filepath.Abs(state)
	if err != nil {
		return "", err
	}
	// Keep below macOS's Unix socket path limit even for long workspace/config paths.
	dir := filepath.Join("/tmp", fmt.Sprintf("codex-gemini-%d", os.Getuid()))
	if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 || !ok || stat.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("socket directory must be owned by this user with mode 0700: %s", dir)
	}
	hash := sha256.Sum256([]byte(abs))
	return filepath.Join(dir, fmt.Sprintf("%x.sock", hash[:12])), nil
}

// Serve requires m to own the state-directory process lock before removing any
// stale socket. Disconnecting one client never cancels another client's jobs.
func Serve(ctx context.Context, state string, m *worker.Manager) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	path, err := SocketPath(state)
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	controlPath := path + ".stop"
	if err = os.Remove(controlPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	control, err := net.ListenUnix("unix", &net.UnixAddr{Name: controlPath, Net: "unix"})
	if err != nil {
		return err
	}
	defer control.Close()
	if err = os.Chmod(controlPath, 0600); err != nil {
		return err
	}
	go func() {
		c, err := control.Accept()
		if err == nil {
			c.Close()
			cancel()
		}
	}()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
		case <-done:
		}
	}()
	server := worker.Server(m)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			disconnected := make(chan struct{})
			defer close(disconnected)
			go func() {
				select {
				case <-ctx.Done():
					conn.Close()
				case <-disconnected:
				}
			}()
			session, err := server.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
			if err != nil {
				return
			}
			session.Wait()
		}()
	}
}

// Stop is a local CLI control action. It cancels work in all connected sessions.
func Stop(ctx context.Context, state string) error {
	path, err := SocketPath(state)
	if err != nil {
		return err
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path+".stop")
	if err != nil {
		return fmt.Errorf("shared service is not running: %w", err)
	}
	return c.Close()
}
