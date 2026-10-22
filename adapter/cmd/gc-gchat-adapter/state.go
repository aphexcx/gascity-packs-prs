package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/ledger"
)

const resetWarning = "messages from the gap may be lost or duplicated"

func runState(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	force := false
	var positional []string
	for _, arg := range args {
		if arg == "--force" && !force {
			force = true
		} else {
			positional = append(positional, arg)
		}
	}
	if len(positional) == 0 || (positional[0] != "reset" && positional[0] != "restore") ||
		(positional[0] == "reset" && len(positional) != 1) || (positional[0] == "restore" && (len(positional) != 2 || strings.HasPrefix(positional[1], "--"))) {
		fmt.Fprintln(stderr, "usage: gc-gchat-adapter state restore <file> [--force] | state reset [--force]")
		fmt.Fprintln(stderr, "socket: GC_SERVICE_SOCKET, or adapter.sock beside the ledger; --force uses the running service's recovery checks")
		return 2
	}
	files, err := resolveFiles(getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg, err := config.Load(files.env, files.settings, files.secrets)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	action, backup := positional[0], ""
	if action == "restore" {
		backup, err = filepath.Abs(positional[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	} else {
		fmt.Fprintln(stderr, "warning: "+resetWarning)
	}
	if err := changeState(ctx, cfg, action, backup, force); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if action == "reset" {
		fmt.Fprintln(stdout, "state reset")
	} else {
		fmt.Fprintln(stdout, "state restored")
	}
	return 0
}

func changeState(ctx context.Context, cfg *config.Config, action, backup string, force bool) error {
	socket := cfg.Secrets.ServiceSocket
	if socket == "" {
		socket = filepath.Join(filepath.Dir(cfg.Endpoints.StateFile), "adapter.sock")
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return fmt.Errorf("state socket directory: %w", err)
	}
	// Holding the same listener address as serve excludes a service startup
	// between a liveness probe and the final atomic ledger replacement.
	ln, err := net.Listen("unix", socket)
	if err != nil {
		if !errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("reserve state socket %s: %w", socket, err)
		}
		if !force {
			return fmt.Errorf("service socket %s is in use; stop the service or use --force to request recovery through the service", socket)
		}
		return requestState(ctx, socket, action, backup)
	}
	defer ln.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return fmt.Errorf("secure state socket: %w", err)
	}
	l := ledger.New()
	if action == "restore" {
		l, err = ledger.Load(backup, true)
		if err != nil {
			return fmt.Errorf("restore backup: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Endpoints.StateFile), 0700); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	if err := l.Save(cfg.Endpoints.StateFile); err != nil {
		return err
	}
	return ledger.MarkEstablished(cfg.SecretsDir)
}

func requestState(ctx context.Context, socket, action, backup string) error {
	data, err := json.Marshal(struct {
		Action string `json:"action"`
		File   string `json:"file,omitempty"`
	}{action, backup})
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/state", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request state recovery via %s: %w", socket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return fmt.Errorf("read state recovery response: %w", err)
		}
		return fmt.Errorf("service state recovery: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
