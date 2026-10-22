// gc-gchat-adapter connects a city's external messages to Google Chat.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/token"
)

// Version is set by the build's -ldflags.
var Version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv, func(url string) error {
		command := "xdg-open"
		if runtime.GOOS == "darwin" {
			command = "open"
		}
		return exec.CommandContext(ctx, command, url).Run()
	})
	stop()
	os.Exit(code)
}

type configFiles struct{ env, settings, secrets string }

// resolveFiles is shared by standalone auth and the eventual serve command.
// Only an absent settings file is optional; other filesystem errors are fatal.
func resolveFiles(getenv func(string) string) (configFiles, error) {
	env := getenv("GC_GCHAT_ADAPTER_ENV")
	if env == "" {
		if dir := getenv("GC_SERVICE_SECRETS_DIR"); dir != "" {
			env = filepath.Join(dir, "env")
		} else {
			home := getenv("HOME")
			if home == "" {
				return configFiles{}, errors.New("resolve env file: HOME is not set; set GC_GCHAT_ADAPTER_ENV or GC_SERVICE_SECRETS_DIR")
			}
			env = filepath.Join(home, ".config", "gc-gchat-adapter", "env")
		}
	}
	files := configFiles{env: env, secrets: filepath.Dir(env)}
	settings := filepath.Join(files.secrets, "..", "config.toml")
	if _, err := os.Stat(settings); err == nil {
		files.settings = settings
	} else if !errors.Is(err, os.ErrNotExist) {
		return configFiles{}, fmt.Errorf("settings %s: %w", settings, err)
	}
	return files, nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, open func(string) error) int {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "version":
		fmt.Fprintf(stdout, "gc-gchat-adapter %s\n", Version)
		return 0
	case "auth":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--check") {
			fmt.Fprintln(stderr, "usage: gc-gchat-adapter auth [--check]")
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
		tokenFile := cfg.Identity().TokenFile
		if len(args) == 2 {
			return checkAuth(ctx, cfg, tokenFile, stdout, stderr)
		}
		err = token.RunAuth(ctx, cfg.Secrets.OAuthClientFile, tokenFile, cfg.Endpoints, token.Scopes, stdout, func(url string) error {
			if open != nil {
				_ = open(url) // The URL is already printed, so manual consent still works.
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "token file %s: %s\n", tokenFile, token.Ready)
		return 0
	case "serve", "state":
		fmt.Fprintln(stderr, "not implemented yet")
		return 2
	default:
		fmt.Fprintln(stderr, "usage: gc-gchat-adapter [serve|auth|state|version]")
		return 2
	}
}

func checkAuth(ctx context.Context, cfg *config.Config, tokenFile string, stdout, stderr io.Writer) int {
	source, err := token.New(cfg.Secrets.OAuthClientFile, tokenFile, cfg.Endpoints, token.Scopes)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// A doctor check must exercise the refresh credential even when the cached
	// access token has not expired. Source owns retries and safe error details.
	source.Invalidate()
	if _, err := source.TokenSource(ctx).Token(); err != nil {
		if errors.Is(err, token.ErrNeedsSignIn) {
			fmt.Fprintf(stdout, "%s: %v\n", token.NeedsSignIn, err)
		} else {
			fmt.Fprintln(stdout, err)
		}
		return 1
	}
	fmt.Fprintln(stdout, source.State())
	return 0
}
