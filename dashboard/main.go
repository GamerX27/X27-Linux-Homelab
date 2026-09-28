// dashboard is the web admin UI for X27-Linux Homelab. One binary runs as either the
// main node (the UI, which also manages paired nodes) or a node (API only, managed from a
// main node). `dashboard serve` is what dashboard.service runs; the other subcommands turn
// it on and off and pair nodes.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/api"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/auth"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/files"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/osupdate"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tlsutil"
	"github.com/gamerx27/x27-linux-homelab/dashboard/web"
)

// Set at build time: -ldflags "-X main.version=2026.09.28".
var version = "dev"

const unit = "dashboard.service"

func usage() {
	fmt.Print(`Usage:
  dashboard enable            turn the dashboard on as the main node (the web UI)
  dashboard enable node       turn it on as a node that a main node manages; prints a pairing password
  dashboard disable           turn it off
  dashboard status            show mode, address, pairing state and certificate fingerprint
  dashboard pair              node: print a new pairing password (pairing again replaces the current main node)
  dashboard unpair            node: forget the main node it's paired with
  dashboard port <port>       change the port (default 9090)
  dashboard nodes             main: list paired nodes
  dashboard serve [--dev]     run the server (what dashboard.service runs)

Log in to the web UI with your Linux username and password; your user must be in the wheel group.
Add a node from the main node's UI with the node's address and its pairing password.
`)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "dashboard: "+format+"\n", a...)
	os.Exit(1)
}

func needRoot() {
	if os.Geteuid() == 0 {
		return
	}
	self, err := os.Executable()
	if err != nil {
		die("%v", err)
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		die("run this as root")
	}
	if err := syscall.Exec(sudo, append([]string{"sudo", self}, os.Args[1:]...), os.Environ()); err != nil {
		die("%v", err)
	}
}

func main() {
	log.SetFlags(0) // journald adds timestamps
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		return
	}
	switch args[0] {
	case "fsop":
		// Internal: one Files-tab operation, run as the logged-in user through runuser.
		os.Exit(files.Fsop(args[1:], os.Stdin, os.Stdout, os.Stderr))
	case "serve":
		serve(args[1:])
	case "enable":
		needRoot()
		mode := config.ModeMain
		if len(args) > 1 {
			mode = args[1]
		}
		enable(mode)
	case "disable":
		needRoot()
		systemctl("disable", "--now", unit)
		fmt.Println("Dashboard off.")
	case "status":
		needRoot()
		status()
	case "pair":
		needRoot()
		pair()
	case "unpair":
		needRoot()
		cfg := load()
		if cfg.Mode != config.ModeNode {
			die("this is a main node, not a node")
		}
		cfg.TokenHash = ""
		save(cfg)
		fmt.Println("Unpaired. The main node can't reach this node anymore; run `dashboard pair` to pair again.")
	case "port":
		needRoot()
		if len(args) < 2 {
			die("usage: dashboard port <port>")
		}
		p, err := strconv.Atoi(args[1])
		if err != nil || !config.ValidPort(p) {
			die("invalid port: %s", args[1])
		}
		cfg := load()
		cfg.Port = p
		save(cfg)
		if active() {
			systemctl("restart", unit)
		}
		fmt.Printf("Port set to %d.\n", p)
	case "nodes":
		needRoot()
		listNodes()
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(1)
	}
}

func load() config.Config {
	cfg, err := config.Load()
	if err != nil {
		die("reading %s: %v", config.Path, err)
	}
	return cfg
}

func save(cfg config.Config) {
	if err := cfg.Save(); err != nil {
		die("writing %s: %v", config.Path, err)
	}
}

func systemctl(args ...string) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		die("systemctl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
}

func active() bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

func cert() tls.Certificate {
	c, err := tlsutil.Ensure(filepath.Join(config.StateDir, "tls"))
	if err != nil {
		die("certificate: %v", err)
	}
	return c
}

func urls(port int) []string {
	var out []string
	for _, ip := range tlsutil.LocalIPs() {
		out = append(out, "https://"+net.JoinHostPort(ip, strconv.Itoa(port)))
	}
	return out
}

func newPairPassword(cfg *config.Config) string {
	pw := config.NewPairPassword()
	cfg.PairHash = config.Hash(pw)
	return pw
}

func printPair(pw string, c tls.Certificate) {
	fmt.Println()
	fmt.Println("Pairing password (works once):")
	fmt.Println()
	fmt.Println("    " + pw)
	fmt.Println()
	fmt.Println("In the main node's dashboard: Add node, then enter this node's address and the password.")
	fmt.Println("The main node shows this certificate fingerprint when it pairs; check that it matches:")
	fmt.Println("    " + tlsutil.Fingerprint(c))
}

func enable(mode string) {
	if mode != config.ModeMain && mode != config.ModeNode {
		die("choose main or node (see: dashboard help)")
	}
	cfg := load()
	changed := cfg.Mode != mode
	cfg.Mode = mode
	var pw string
	if mode == config.ModeMain {
		cfg.PairHash, cfg.TokenHash = "", ""
	} else if cfg.TokenHash == "" || changed {
		cfg.TokenHash = ""
		pw = newPairPassword(&cfg)
	}
	save(cfg)
	c := cert()
	systemctl("enable", unit)
	systemctl("restart", unit)
	if mode == config.ModeMain {
		fmt.Println("Dashboard on (main node). Open it at:")
		for _, u := range urls(cfg.Port) {
			fmt.Println("    " + u)
		}
		fmt.Println("Your browser warns about the self-signed certificate the first time. Its fingerprint:")
		fmt.Println("    " + tlsutil.Fingerprint(c))
		fmt.Println("Log in with your Linux username and password (the user must be in the wheel group).")
		return
	}
	fmt.Printf("Dashboard on (node), listening on port %d.\n", cfg.Port)
	if pw != "" {
		printPair(pw, c)
	} else {
		fmt.Println("Already paired with a main node. Run `dashboard pair` to pair with another one.")
	}
}

func pair() {
	cfg := load()
	if cfg.Mode != config.ModeNode {
		die("this is a main node; run `dashboard enable node` to make it a node")
	}
	pw := newPairPassword(&cfg)
	save(cfg)
	if cfg.TokenHash != "" {
		fmt.Println("This node is paired already. Pairing with this password replaces that main node.")
	}
	printPair(pw, cert())
	if !active() {
		fmt.Println("\nThe dashboard isn't running; start it with: dashboard enable node")
	}
}

func status() {
	cfg := load()
	enabled, _ := exec.Command("systemctl", "is-enabled", unit).Output()
	state := "off"
	if active() {
		state = "running"
	} else if strings.TrimSpace(string(enabled)) == "enabled" {
		state = "enabled, not running (see: journalctl -u dashboard)"
	}
	fmt.Printf("Dashboard:   %s\n", state)
	fmt.Printf("Mode:        %s\n", cfg.Mode)
	fmt.Printf("Port:        %d\n", cfg.Port)
	for i, u := range urls(cfg.Port) {
		label := "Address:"
		if i > 0 {
			label = ""
		}
		fmt.Printf("%-12s %s\n", label, u)
	}
	if cfg.Mode == config.ModeNode {
		paired := "no"
		if cfg.TokenHash != "" {
			paired = "yes"
		}
		fmt.Printf("Paired:      %s\n", paired)
		if cfg.PairHash != "" {
			fmt.Println("Pairing:     waiting (a pairing password is active)")
		}
	}
	if c, err := tlsutil.Ensure(filepath.Join(config.StateDir, "tls")); err == nil {
		fmt.Printf("Fingerprint: %s\n", tlsutil.Fingerprint(c))
	}
}

func listNodes() {
	s, err := nodes.Open()
	if err != nil {
		die("%v", err)
	}
	list := s.List()
	if len(list) == 0 {
		fmt.Println("No paired nodes. Add one in the dashboard: Add node.")
		return
	}
	for _, n := range list {
		fmt.Printf("%-20s %-24s paired %s\n", n.Name, n.Address, time.Unix(n.AddedAt, 0).Format("2006-01-02"))
	}
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dev := fs.Bool("dev", false, "development: no PAM (any password), shell as yourself, localhost only, main over plain HTTP")
	mode := fs.String("mode", "", "override MODE from the config file")
	port := fs.Int("port", 0, "override PORT from the config file")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("reading %s: %v", config.Path, err)
	}
	if *mode != "" {
		cfg.Mode = *mode
	}
	if *port != 0 {
		cfg.Port = *port
	}
	c, err := tlsutil.Ensure(filepath.Join(config.StateDir, "tls"))
	if err != nil {
		log.Fatalf("certificate: %v", err)
	}
	osupdate.Fake = *dev && os.Getenv("DASHBOARD_FAKE_OS") == "1"
	opts := api.Options{Version: version, Mode: cfg.Mode, Dev: *dev, Cert: c, Web: web.FS, Auth: auth.PAM, Presets: cfg.Presets}
	if *dev {
		opts.Auth = func(u, p string) error {
			if u == "" || p == "" {
				return auth.ErrDenied
			}
			return nil
		}
	}

	var h http.Handler
	switch cfg.Mode {
	case config.ModeMain:
		if h, err = api.NewMain(opts); err != nil {
			log.Fatal(err)
		}
	case config.ModeNode:
		h = api.NewNode(opts)
	default:
		log.Fatalf("unknown mode %q", cfg.Mode)
	}

	host := ""
	if *dev {
		host = "127.0.0.1"
	}
	srv := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(cfg.Port)),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12},
	}
	log.Printf("dashboard %s (%s) listening on %s, certificate %s", version, cfg.Mode, srv.Addr, tlsutil.Fingerprint(c))
	if *dev && cfg.Mode == config.ModeMain {
		log.Printf("dev mode: http://%s", srv.Addr)
		log.Fatal(srv.ListenAndServe())
	}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
