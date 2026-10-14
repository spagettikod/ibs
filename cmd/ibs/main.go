// Command ibs runs an IBS site manager or a storage node.
//
//	ibs manager         (default) site manager: admin UI, S3 endpoint, peer listener
//	ibs node [token]    storage node; the token from the manager's Nodes page is
//	                    only needed for the first start
//	ibs version         print the version
//	ibs recovery-phrase print the site's recovery phrase (from the data directory)
//	ibs reset-password  set a new admin password: ibs reset-password <password>
//	ibs https-off       remove the HTTPS settings made in the web interface
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"ibs/internal/config"
	"ibs/internal/node"
	"ibs/internal/release"
	"ibs/internal/s3api"
	"ibs/internal/site"
	"ibs/internal/tlscert"
	"ibs/internal/version"
	"ibs/internal/web"
	webui "ibs/web"
)

func main() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("IBS_LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 {
		cfg.Mode = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Mode == "manager" || cfg.Mode == "node" {
		slog.Info("starting IBS", "version", version.Version, "mode", cfg.Mode)
		// So that launchers built before this version trust its release keys.
		if err := (release.Store{Dir: cfg.DataDir}).SaveBuiltinKeys(); err != nil {
			slog.Warn("save release keys", "err", err)
		}
	}
	switch cfg.Mode {
	case "manager":
		err = runManager(ctx, cfg)
		if errors.Is(err, errRestart) {
			os.Exit(site.RestartCode) // after runManager closed the site
		}
	case "node":
		token := ""
		if len(os.Args) > 2 {
			token = os.Args[2]
		}
		err = node.Run(ctx, cfg, token)
		if errors.Is(err, node.ErrRestart) {
			os.Exit(site.RestartCode) // the launcher starts the new version
		}
	case "version":
		fmt.Println("ibs", version.Version)
	case "recovery-phrase":
		var phrase string
		if phrase, err = site.RecoveryPhrase(cfg.DataDir); err == nil {
			fmt.Println(phrase)
		}
	case "https-off":
		if len(cfg.TLSDomains) > 0 {
			err = errors.New("HTTPS is set with IBS_TLS_* environment variables; remove them instead")
		} else if err = tlscert.Disable(filepath.Join(cfg.DataDir, "tls")); err == nil {
			fmt.Println("HTTPS settings removed; restart the manager")
		}
	case "reset-password":
		if len(os.Args) != 3 {
			err = errors.New("usage: ibs reset-password <new password>")
		} else if err = site.ResetPassword(cfg.DataDir, os.Args[2]); err == nil {
			fmt.Println("admin password changed")
		}
	default:
		err = fmt.Errorf("unknown command %q (use manager, node, version, recovery-phrase, reset-password or https-off)", cfg.Mode)
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// serve runs srv until ctx ends, with HTTPS on the same port once certs has
// a certificate.
func serve(ctx context.Context, certs *tlscert.Manager, srv *http.Server) {
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		slog.Error("listen", "addr", srv.Addr, "err", err)
		os.Exit(1)
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	go func() {
		if err := srv.Serve(certs.Listener(ln)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "addr", srv.Addr, "err", err)
			os.Exit(1)
		}
	}()
}

// errRestart: the manager switched versions; the launcher starts the new one.
var errRestart = errors.New("restart into a new version")

func runManager(ctx context.Context, cfg config.Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// HTTPS for the web interface and S3, set in the web interface or, taking
	// precedence, with IBS_TLS_* variables.
	var envTLS *tlscert.Settings
	if len(cfg.TLSDomains) > 0 {
		envTLS = &tlscert.Settings{
			Domains: cfg.TLSDomains, Provider: cfg.TLSDNSProvider, Email: cfg.TLSEmail, CA: cfg.TLSCA,
			Resolvers: cfg.TLSDNSResolvers, PropagationWait: cfg.TLSDNSWait, Require: cfg.TLSRequire,
		}
	}
	certs, err := tlscert.New(filepath.Join(cfg.DataDir, "tls"), envTLS)
	if err != nil {
		return fmt.Errorf("HTTPS: %w", err)
	}
	go certs.Run(ctx)

	var once sync.Once
	start := func(st *site.Site) {
		once.Do(func() {
			// HTTPS state travels with the site's metadata: restore it after
			// a restore, and keep the backup up to date.
			if ok, err := certs.Import(st.TLSBackup()); err != nil {
				slog.Warn("restore HTTPS settings", "err", err)
			} else if ok {
				slog.Info("HTTPS settings restored")
			}
			backup := func() {
				if b, err := certs.Export(); err != nil {
					slog.Warn("back up HTTPS settings", "err", err)
				} else if err := st.SetTLSBackup(b); err != nil {
					slog.Warn("back up HTTPS settings", "err", err)
				}
			}
			certs.OnChange(backup)
			backup()
			st.Run(ctx)
			if err := st.Serve(ctx); err != nil {
				slog.Error("site network", "err", err)
				os.Exit(1)
			}
			serve(ctx, certs, &http.Server{
				Addr:              cfg.S3Addr,
				Handler:           certs.Guard(&s3api.Handler{Site: st, Domain: cfg.S3Domain}, true),
				ReadHeaderTimeout: 30 * time.Second,
			})
			slog.Info("site manager running", "site", st.Name(), "site_id", st.ID.SiteID,
				"s3", cfg.S3Addr, "web", cfg.WebAddr, "peer", cfg.PeerAddr)
		})
	}

	var st *site.Site
	switch {
	case site.IsSetUp(cfg.DataDir):
		if st, err = site.Open(cfg); err != nil {
			return err
		}
	case cfg.SiteName != "" && cfg.AdminPassword != "":
		var phrase string
		if st, phrase, err = site.Create(cfg, cfg.SiteName, cfg.AdminPassword); err != nil {
			return err
		}
		slog.Warn("NEW SITE CREATED. Write down the recovery phrase; it is needed to restore this site and is not shown again.",
			"recovery_phrase", phrase)
	default:
		slog.Info("site is not set up; open the web interface to create or restore it", "web", cfg.WebAddr)
	}
	if st != nil {
		defer st.Close()
		start(st)
	}
	admin := web.New(cfg, st, webui.Assets(), start)
	admin.TLS = certs
	serve(ctx, certs, &http.Server{
		Addr:              cfg.WebAddr,
		Handler:           certs.Guard(admin, false),
		ReadHeaderTimeout: 30 * time.Second,
	})
	restart := false
	select {
	case <-ctx.Done():
	case <-site.RestartRequested:
		restart = true
		cancel()
	}
	slog.Info("shutting down", "restart", restart)
	time.Sleep(500 * time.Millisecond)
	if restart {
		return errRestart
	}
	return nil
}
