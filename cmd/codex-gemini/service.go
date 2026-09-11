package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/impulse-ai/codex-gemini/internal/service"
	"github.com/impulse-ai/codex-gemini/internal/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func defaultStateDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "codex-gemini", "state"), nil
}

func connectService(ctx context.Context, state string, args []string) (net.Conn, error) {
	if err := os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	path, err := service.SocketPath(state)
	if err != nil {
		return nil, err
	}
	dial := func() (net.Conn, error) { return net.DialTimeout("unix", path, 200*time.Millisecond) }
	if c, err := dial(); err == nil {
		return c, nil
	}
	lock, err := os.OpenFile(filepath.Join(state, "startup.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if c, err := dial(); err == nil {
		return c, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(state, "service.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	cmd := exec.Command(executable, append([]string{"daemon"}, args...)...)
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for {
		if c, err := dial(); err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("service startup timed out; inspect %s", filepath.Join(state, "service.log"))
		case err := <-exited:
			return nil, fmt.Errorf("service exited (%v); inspect %s", err, filepath.Join(state, "service.log"))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func proxyStdio(ctx context.Context, conn net.Conn) error {
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if unix, ok := conn.(*net.UnixConn); ok {
			unix.CloseWrite()
		}
	}()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-finished:
		}
	}()
	_, err := io.Copy(os.Stdout, conn)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func cancelCLIJobs(state string, jobs []worker.Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path, err := service.SocketPath(state)
	if err != nil {
		return
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return
	}
	defer conn.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "codex-gemini-cancel", Version: "0.6.0"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		return
	}
	defer session.Close()
	for _, job := range jobs {
		_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "gemini_cancel", Arguments: worker.IDInput{ID: job.ID}})
	}
}
