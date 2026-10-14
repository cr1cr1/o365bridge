package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/cr1cr1/o365bridge/internal/bridge"
	"github.com/cr1cr1/o365bridge/internal/davserver"
	"github.com/cr1cr1/o365bridge/internal/graph"
	"github.com/cr1cr1/o365bridge/internal/imapserver"
	"github.com/cr1cr1/o365bridge/internal/jmapadapter"
	"github.com/cr1cr1/o365bridge/internal/owa"
	"github.com/cr1cr1/o365bridge/internal/rodlogin"
	"github.com/cr1cr1/o365bridge/internal/smtpserver"
)

// ServeCmd starts the bridge: plain IMAP/SMTP/CalDAV/CardDAV
// listeners on loopback by default, accounts discovered dynamically
// when a mail client first presents an email address. All settings
// are flags; Kong also exposes each as an OB_<FLAG> envar. There is
// no config file.
type ServeCmd struct {
	Interface     string `help:"Interface/address to bind all listeners to" default:"127.0.0.1"`
	ImapPort      int    `help:"IMAP listen port" default:"1143"`
	SmtpPort      int    `help:"SMTP submission listen port" default:"1025"`
	DavPort       int    `help:"CalDAV/CardDAV HTTP listen port" default:"1080"`
	JmapPort      int    `help:"JMAP HTTP listen port (0 disables)" default:"8080"`
	Password      string `help:"Bridge instance password clients use with their email as username (prefer the OB_PASSWORD envar over the flag)"`
	GraphClientID string `help:"Admin-created Azure app client ID; empty disables Graph and uses OWA directly" default:""`
	StateDir      string `help:"Session cache directory (accounts, browser profiles, ID maps)" default:""`
	BrowserPath   string `help:"Chromium/Chrome binary for login windows; empty = auto-discover/download" default:""`
}

// resolveStateDir picks the per-user state directory:
// $XDG_STATE_HOME/o365bridge, else ~/.local/state/o365bridge.
func resolveStateDir(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, appName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", appName), nil
}

// Run starts all listeners and blocks until SIGINT/SIGTERM.
func (s *ServeCmd) Run(d *Deps) error {
	if s.Password == "" {
		return &ExitError{Code: 2, Err: fmt.Errorf("bridge instance password is required: pass --password or set OB_PASSWORD")}
	}
	stateDir, err := resolveStateDir(s.StateDir)
	if err != nil {
		return err
	}

	login := rodlogin.New(stateDir, s.GraphClientID, log.Logger)
	login.BrowserPath = s.BrowserPath

	graphFactory := graph.Factory{}
	owaFactory := owa.Factory{}
	factory := bridge.FactoryFunc(func(email string, sess bridge.Session) bridge.Provider {
		if sess.Channel == bridge.ChannelGraph {
			return graphFactory.New(email, sess)
		}
		return owaFactory.New(email, sess)
	})

	reg, err := bridge.NewRegistry(bridge.Config{
		StateDir:      stateDir,
		GraphClientID: s.GraphClientID,
		Login:         login,
		Factory:       factory,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	serve := func(name string, port int, fn func(net.Listener) error) {
		addr := net.JoinHostPort(s.Interface, fmt.Sprint(port))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			errCh <- fmt.Errorf("listen %s on %s: %w", name, addr, err)
			return
		}
		log.Info().Str("protocol", name).Str("listen", addr).Msg("listener up")
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(l); err != nil {
				errCh <- fmt.Errorf("%s server: %w", name, err)
			}
		}()
	}

	imapSrv := imapserver.New(reg, s.Password, log.Logger)
	smtpSrv := smtpserver.New(reg, s.Password, log.Logger)
	davSrv := &http.Server{
		Handler:           davserver.New(reg, s.Password, log.Logger),
		ReadHeaderTimeout: 30 * time.Second,
	}
	var jmapSrv *http.Server
	if s.JmapPort != 0 {
		engine, _ := jmapadapter.NewEngine(reg, s.Password, log.Logger)
		jmapSrv = &http.Server{
			Handler:           engine.Handler(),
			ReadHeaderTimeout: 30 * time.Second,
		}
	}

	serve("imap", s.ImapPort, imapSrv.Serve)
	serve("smtp", s.SmtpPort, smtpSrv.Serve)
	serve("dav", s.DavPort, davSrv.Serve)
	if jmapSrv != nil {
		serve("jmap", s.JmapPort, jmapSrv.Serve)
	}

	if s.GraphClientID == "" {
		log.Info().Msg("graph disabled (no client ID configured); accounts will use OWA")
	} else {
		log.Info().Msg("graph enabled; accounts probe Graph first, then OWA")
	}
	log.Info().Str("state", stateDir).Msg("o365bridge serving; add accounts from your mail client")

	select {
	case <-ctx.Done():
		log.Info().Msg("shutting down")
	case err := <-errCh:
		stop()
		_ = imapSrv.Close()
		_ = smtpSrv.Close()
		_ = davSrv.Close()
		if jmapSrv != nil {
			_ = jmapSrv.Close()
		}
		wg.Wait()
		return err
	}

	_ = imapSrv.Close()
	_ = smtpSrv.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = davSrv.Shutdown(shutdownCtx)
	if jmapSrv != nil {
		_ = jmapSrv.Shutdown(shutdownCtx)
	}
	wg.Wait()
	return nil
}
