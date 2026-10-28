// Package app is bnbloc-agent-manager's command line and server is the agent manager: an installable plugin
// component (docs/prd/2026-10-24-agent-manager.md in bnbloc) that shows the
// coding agents on a member's computer, installs the ones its signed catalog
// can, keeps them current and gives them to the ACP core. Nothing is installed
// or changed until the member asks, unless they turned automatic updates on.
//
// As a plugin sidecar it serves a loopback API behind a per-launch bearer; the
// member's page (a bundle) reaches it through the desktop. The environment is
// the one the package's sidecar block names:
//
//	BNBLOC_AGENT_MANAGER_LISTEN_ADDR  loopback host:port to listen on
//	BNBLOC_AGENT_MANAGER_TOKEN        the bearer every request must carry
//	BNBLOC_AGENT_MANAGER_DATA_DIR     the private folder (state, installed agents)
//	BNBLOC_AGENT_MANAGER_SUPERVISED   set when the desktop owns stdin (exit on EOF)
//	BNBLOC_AGENT_MANAGER_ACP_HOME     the ACP core's home (default: its usual place)
//	BNBLOC_AGENT_MANAGER_MOUNTS_FILE  the file the sandbox bridge reads this app's mounts from
//	BNBLOC_AGENT_MANAGER_MOUNT_ROOT   the folder inside the workspaces root this app may fill
//	                                  (both set by the desktop; without them no agent goes to the sandbox)
//
// and the platform's own BNBLOC_COMPONENT_SETTINGS_FILE.
//
// The same binary is a command line for the same work:
//
//	bnbloc-agent-manager list
//	bnbloc-agent-manager check | install | install-sandbox | update | rollback | remove | register | unregister <id>
//	bnbloc-agent-manager serve [--page]   (the loopback API; --page also serves the page at /)
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/luckylhb90/bnbloc-agent-manager/manager"
)

const (
	envListen     = "BNBLOC_AGENT_MANAGER_LISTEN_ADDR"
	envToken      = "BNBLOC_AGENT_MANAGER_TOKEN"
	envDataDir    = "BNBLOC_AGENT_MANAGER_DATA_DIR"
	envSupervised = "BNBLOC_AGENT_MANAGER_SUPERVISED"
	envACPHome    = "BNBLOC_AGENT_MANAGER_ACP_HOME"
	envMounts     = "BNBLOC_AGENT_MANAGER_MOUNTS_FILE"
	envMountRoot  = "BNBLOC_AGENT_MANAGER_MOUNT_ROOT"
	envSettings   = "BNBLOC_COMPONENT_SETTINGS_FILE"
)

// Main runs the agent manager: the command line or the loopback server, over
// the catalog the build was given and under the version it reports. The public
// build passes its own catalog; an organization's build passes that catalog
// with its own entries added (Catalog.With), signed with its own package.
func Main(args []string, cat *manager.Catalog, version string) error {
	if cat == nil {
		return fmt.Errorf("no catalog")
	}
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dataDir := fs.String("data-dir", os.Getenv(envDataDir), "the private folder (default: the user config dir)")
	acpHome := fs.String("acp-home", os.Getenv(envACPHome), "the ACP core's home (default: its usual place)")
	settings := fs.String("settings", os.Getenv(envSettings), "the component settings file")
	page := fs.Bool("page", false, "serve: also serve the page at /")
	listen := fs.String("listen", os.Getenv(envListen), "serve: loopback host:port")
	// Flags may come before or after the agent id.
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
	svc, err := newService(cat, version, *dataDir, *acpHome, *settings)
	if err != nil {
		return err
	}
	switch cmd {
	case "serve":
		return serve(svc, version, *listen, *page)
	case "list":
		svc.RefreshDetected(context.Background())
		svc.CheckAll(context.Background())
		return list(os.Stdout, svc)
	case manager.ActCheck, manager.ActInstall, manager.ActInstallSandbox, manager.ActUpdate, manager.ActRollback, manager.ActRemove, manager.ActRegister, manager.ActUnregister:
		if len(rest) != 1 {
			return fmt.Errorf("usage: bnbloc-agent-manager %s <agent id>", cmd)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		svc.RefreshDetected(ctx)
		err := svc.Do(ctx, rest[0], cmd, func(l string) { fmt.Println(l) })
		_ = svc.WriteRegistration()
		return err
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func newService(cat *manager.Catalog, version, dataDir, acpHome, settings string) (*manager.Service, error) {
	if dataDir == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		dataDir = filepath.Join(cfg, "bnbloc-agent-manager")
	}
	if acpHome == "" {
		if cfg, err := os.UserConfigDir(); err == nil {
			acpHome = filepath.Join(cfg, "bnbloc-acp")
		}
	}
	svc := &manager.Service{
		Catalog: cat, Store: manager.Store{Dir: dataDir}, SettingsFile: settings,
		ACPHome: acpHome, Version: version,
		MountRoot: os.Getenv(envMountRoot), MountsFile: os.Getenv(envMounts),
	}
	if svc.MountRoot != "" && svc.MountsFile != "" {
		// The guest sees the member's home path as its mount root (the bridge's
		// default allowed folder); the agents appear under a folder of ours.
		if home, err := os.UserHomeDir(); err == nil {
			svc.GuestBase = filepath.Join(home, ".bnbloc-agents")
		}
	}
	if err := svc.Init(); err != nil {
		return nil, err
	}
	return svc, nil
}

func serve(svc *manager.Service, version, listen string, withPage bool) error {
	token := os.Getenv(envToken)
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	if err := loopbackOnly(listen); err != nil {
		return err
	}
	if token == "" {
		if !withPage {
			return fmt.Errorf("%s is not set: this bnbloc desktop is too old to start a panel component; update the app", envToken)
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		token = hex.EncodeToString(b) // development: the page takes it from the URL
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if os.Getenv(envSupervised) != "" {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			stop() // the desktop is gone: shut down
		}()
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: svc.Handler(token, withPage), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	go svc.Run(ctx)
	if withPage {
		log.Printf("bnbloc-agent-manager %s: open http://%s/#token=%s", version, ln.Addr(), token)
	} else {
		log.Printf("bnbloc-agent-manager %s: serving on %s", version, ln.Addr())
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loopbackOnly refuses any address but loopback: the bearer travels in the clear.
func loopbackOnly(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("%s must be host:port, got %q", envListen, addr)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s must be a loopback address, got %q", envListen, host)
	}
	return nil
}

func list(w io.Writer, svc *manager.Service) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tINSTALLED\tLATEST\tHOW")
	for _, a := range svc.Snapshot().Agents {
		installed, how := "-", ""
		switch {
		case a.Managed != nil:
			installed, how = a.Managed.Version, "managed"
		case a.External != nil:
			installed, how = orDash(a.External.Version), "found: "+a.External.Kind
		}
		state := a.Status
		if a.UpdateAvailable {
			state = "update available"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, state, installed, orDash(a.Latest), how)
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
