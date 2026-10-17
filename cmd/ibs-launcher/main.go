// Command ibs-launcher starts the IBS program and keeps it up to date. It is
// what the ibs-launcher container image runs; all arguments and settings are
// passed on to IBS unchanged (ibs-launcher node <token> runs a storage node,
// and so on). It is a program of its own, versioned separately from IBS.
//
// It runs the version recorded as current in the data directory, after
// checking its signature and that the program on disk is the one signed. When
// there is none yet, it takes IBS_BINARY (a local .ibsrelease file); else a
// storage node gets the version its manager runs, from the manager (nodes never
// go to the internet), and a manager downloads the latest release from GitHub. IBS installs updates itself and
// then exits with code 75; the launcher starts the new version. If a new
// version stops within its first minutes, the launcher marks it bad and goes
// back to the last version that ran fine.
//
//	IBS_DATA          data directory (default /data)
//	IBS_TRUSTED_KEYS  release keys to trust besides the built-in one (ibspub1:..., comma-separated)
//	IBS_BINARY        a local .ibsrelease file to run, signed by a trusted key
//	IBS_RELEASES_URL  where to look for releases (default GitHub's list of IBS releases)
//	IBS_BETA          true: the first download may be a beta (default: the latest stable
//	                  release); IBS also starts with betas counting as new versions
//	IBS_PROBATION     how long a new version must run before it counts as good (default 2m)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"syscall"
	"time"

	"ibs/internal/config"
	"ibs/internal/node"
	"ibs/internal/release"
	"ibs/internal/stall"
)

// Version is the launcher's own version; see CHANGELOG.md next to this file.
var Version = "1.1.0"

// RestartCode is the exit code IBS uses to ask for the current version to be
// started (after it installed an update or a rollback).
const RestartCode = 75

var log = slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "launcher")

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "launcher-version" {
		fmt.Println("ibs-launcher", Version)
		return
	}
	log.Info("IBS launcher", "version", Version)
	store := release.Store{Dir: env("IBS_DATA", "/data")}
	if err := store.SaveBuiltinKeys(); err != nil {
		fatal(err)
	}
	envKeys, err := release.ParseKeyList(os.Getenv("IBS_TRUSTED_KEYS"))
	if err != nil {
		fatal(fmt.Errorf("IBS_TRUSTED_KEYS: %w", err))
	}
	for _, key := range envKeys {
		if slices.ContainsFunc(store.Keys(), func(t release.Key) bool { return t.ID == key.ID }) {
			continue // already trusted; keep the name it was given
		}
		if _, err := store.AddKey(key.String(), "From IBS_TRUSTED_KEYS"); err != nil {
			fatal(fmt.Errorf("IBS_TRUSTED_KEYS: %w", err))
		}
		log.Info("trusting release key", "key", key.ID)
	}
	if err := seed(store); err != nil {
		fatal(err)
	}
	probation, err := time.ParseDuration(env("IBS_PROBATION", "2m"))
	if err != nil {
		fatal(fmt.Errorf("IBS_PROBATION: %w", err))
	}
	os.Exit(run(store, probation))
}

// seed makes sure there is a current version: IBS_BINARY if given (also when
// it is newer than the current one), otherwise the manager's version for a
// storage node, or the latest release on GitHub for a manager.
func seed(store release.Store) error {
	if path := os.Getenv("IBS_BINARY"); path != "" {
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("IBS_BINARY: %w", err)
		}
		b, err := release.ReadTar(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("IBS_BINARY: %w", err)
		}
		in, err := store.Install(b, store.Keys())
		if err != nil {
			return fmt.Errorf("IBS_BINARY: %w", err)
		}
		if cur := store.Current(); cur == "" || release.Compare(in.Version, cur) > 0 {
			log.Info("using local release", "version", in.Version, "key", in.KeyID)
			store.SetCurrent(in.Version)
		}
	}
	if store.Current() != "" {
		return nil
	}
	if nodeMode() {
		// The manager may still be starting; keep asking it.
		for {
			err := fromManager(store)
			if err == nil {
				return nil
			}
			log.Warn("cannot get IBS from the manager yet; trying again in 10s", "err", err)
			time.Sleep(10 * time.Second)
		}
	}
	log.Info("no IBS version installed yet; downloading the latest release")
	b, err := downloadLatest(release.ReleasesURL())
	if err != nil {
		return fmt.Errorf("download IBS (or set IBS_BINARY to a local release): %w", err)
	}
	in, err := store.Install(b, store.Keys())
	if err != nil {
		return fmt.Errorf("downloaded release: %w", err)
	}
	log.Info("downloaded IBS", "version", in.Version, "key", in.KeyID)
	return store.SetCurrent(in.Version)
}

// nodeMode reports whether this runs a storage node (ibs-launcher node <token>,
// or IBS_MODE=node).
func nodeMode() bool {
	return (len(os.Args) > 1 && os.Args[1] == "node") || os.Getenv("IBS_MODE") == "node"
}

// fromManager installs the version the node's manager runs, trusting the
// release keys the manager trusts: the manager is pinned by the key in the
// join token, so it is the authority on what its nodes run.
func fromManager(store release.Store) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	token := ""
	if len(os.Args) > 2 && os.Args[1] == "node" {
		token = os.Args[2]
	}
	// No total time limit: the download fails when it makes no progress.
	b, keys, err := node.FetchRelease(context.Background(), cfg, token)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := store.AddKey(k, "from the manager"); err != nil {
			return err
		}
	}
	in, err := store.Install(b, store.Keys())
	if err != nil {
		return err
	}
	log.Info("got IBS from the manager", "version", in.Version, "key", in.KeyID)
	return store.SetCurrent(in.Version)
}

// downloadLatest downloads the newest stable release, or with IBS_BETA=true
// the newest release, beta or not.
func downloadLatest(url string) (release.Bundle, error) {
	betas, _ := strconv.ParseBool(os.Getenv("IBS_BETA"))
	ctx := context.Background() // requests fail when they make no progress (get)
	resp, err := get(ctx, url)
	if err != nil {
		return release.Bundle{}, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		return release.Bundle{}, err
	}
	rel, err := release.PickGitHubRelease(body, betas)
	if errors.Is(err, release.ErrNoRelease) && !betas {
		return release.Bundle{}, fmt.Errorf("%w (set IBS_BETA=true to run a beta)", err)
	}
	if err != nil {
		return release.Bundle{}, err
	}
	log.Info("found release", "version", rel.Version)
	if resp, err = get(ctx, rel.AssetURL); err != nil {
		return release.Bundle{}, err
	}
	defer resp.Body.Close()
	return release.ReadTar(resp.Body)
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream, application/json")
	resp, err := stall.Do(http.DefaultClient, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp, nil
}

// run starts the current version and keeps it running; see the package comment.
func run(store release.Store, probation time.Duration) int {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	stopping := false
	for {
		cur := store.Current()
		if _, err := store.Verify(cur, store.Keys()); err != nil {
			log.Error("installed version doesn't verify", "version", cur, "err", err)
			if fallback(store, cur) == "" && !fresh(store) {
				return 1
			}
			continue
		}
		cmd := exec.Command(store.Program(cur), os.Args[1:]...)
		cmd.Env = append(os.Environ(), "IBS_LAUNCHER="+Version)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		log.Info("starting IBS", "version", cur)
		started := time.Now()
		if err := cmd.Start(); err != nil {
			log.Error("cannot start IBS", "version", cur, "err", err)
			if fallback(store, cur) == "" {
				return 1
			}
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var good <-chan time.Time
		if store.Good() != cur {
			good = time.After(probation)
		}
	wait:
		for {
			select {
			case s := <-sigs:
				stopping = true
				cmd.Process.Signal(s)
			case <-good:
				store.SetGood(cur)
				log.Info("version runs fine", "version", cur)
				good = nil
			case err := <-done:
				code := exitCode(err)
				switch {
				case stopping:
					return code
				case code == RestartCode:
					log.Info("IBS asked to restart", "next", store.Current())
				case good != nil && time.Since(started) < probation:
					log.Error("new version stopped during its first minutes; going back", "version", cur, "exit", code)
					if fallback(store, cur) == "" {
						return code
					}
				default:
					return code
				}
				break wait
			}
		}
	}
}

// fresh installs the latest release when no installed version can run, for
// example after their key was revoked. It reports whether that worked.
func fresh(store release.Store) bool {
	if nodeMode() {
		err := fromManager(store)
		if err != nil {
			log.Error("cannot get IBS from the manager", "err", err)
		}
		return err == nil
	}
	log.Info("no installed version can run; downloading the latest release")
	b, err := downloadLatest(release.ReleasesURL())
	if err == nil {
		var in release.Installed
		if in, err = store.Install(b, store.Keys()); err == nil {
			err = store.SetCurrent(in.Version)
		}
	}
	if err != nil {
		log.Error("cannot download IBS", "err", err)
		return false
	}
	return true
}

// fallback marks v bad and switches to the version to go back to; it returns
// that version, or "" when there is none.
func fallback(store release.Store, v string) string {
	store.MarkBad(v)
	if back := store.Current(); back != v {
		log.Info("going back", "to", back)
		return back
	}
	return ""
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

func fatal(err error) {
	log.Error("cannot start", "err", err)
	os.Exit(1)
}
