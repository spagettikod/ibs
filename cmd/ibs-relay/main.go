// Command ibs-relay runs an IBS relay: a small service on a reachable
// machine that lets sites connect to each other without exposing their own
// ports. It stores no site data, only its key and the keys of registered
// sites.
//
//	ibs-relay            run the relay
//	ibs-relay token      print the token sites need to use this relay
//	ibs-relay new-token  replace the token; registered sites keep working
//	ibs-relay version    print the version
//
//	IBS_RELAY_ADDR       listen address, TCP and UDP (default :7444)
//	IBS_RELAY_ADVERTISE  optional: host:port sites use to reach the relay, shown in the log
//	IBS_RELAY_SECRET     registration secret (default: generated and kept)
//	IBS_RELAY_TOKEN_OUT  also write the relay token to this file (automation)
//	IBS_DATA             state directory (default /data)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"ibs/internal/relay"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func port(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil || p == "" {
		return "7444"
	}
	return p
}

func main() {
	dir := env("IBS_DATA", "/data")
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println("ibs-relay", relay.Version)
		case "token":
			token, err := relay.ReadToken(dir)
			exit(err, "the relay has not run yet, or IBS_DATA is wrong")
			fmt.Println(token)
		case "new-token":
			if os.Getenv("IBS_RELAY_SECRET") != "" {
				exit(fmt.Errorf("IBS_RELAY_SECRET is set"), "change it there and restart the relay instead")
			}
			token, err := relay.NewToken(dir)
			exit(err, "the relay has not run yet, or IBS_DATA is wrong")
			fmt.Println(token)
			fmt.Fprintln(os.Stderr, "Sites already using this relay keep working and receive the new token automatically.\nThe old token can no longer add new sites.")
		default:
			exit(fmt.Errorf("unknown command %q", os.Args[1]), "use token, new-token or version, or no command to run the relay")
		}
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	slog.Info("starting IBS relay", "version", relay.Version)
	addr := env("IBS_RELAY_ADDR", ":7444")
	advertise := os.Getenv("IBS_RELAY_ADVERTISE")
	srv, err := relay.New(dir, advertise, os.Getenv("IBS_RELAY_SECRET"))
	if err != nil {
		slog.Error("start relay", "err", err)
		os.Exit(1)
	}
	if advertise == "" {
		advertise = "<this machine's public address>:" + port(addr)
	}
	slog.Info("add this relay to a site under IBS network > Add relay; print its token with: ibs-relay token",
		"address", advertise)
	if out := os.Getenv("IBS_RELAY_TOKEN_OUT"); out != "" {
		write := func(token string) {
			if err := os.WriteFile(out, []byte(token+"\n"), 0o644); err != nil {
				slog.Warn("write relay token", "err", err)
			}
		}
		write(srv.Token())
		srv.OnTokenChange = write
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, addr); err != nil {
		slog.Error("relay stopped", "err", err)
		os.Exit(1)
	}
}

func exit(err error, hint string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibs-relay: %v (%s)\n", err, hint)
		os.Exit(1)
	}
}
