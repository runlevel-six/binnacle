// Command sextant is a terminal dashboard for Cluster API on bare metal.
//
// It watches a management cluster — Cluster API, Metal3, BareMetalHost — and the
// workload cluster it provisions, and presents a rolling upgrade and the
// cluster's reaction to it on a single screen.
//
// Running it with no arguments watches the kubeconfig's current context.
// --list-contexts shows what the resolver sees, --debug-snapshot verifies the
// data layer against a cluster without starting the interface, and --dry-run
// reports what would be watched.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/runlevel-six/binnacle/internal/app"
	"github.com/runlevel-six/binnacle/internal/build"
	"github.com/runlevel-six/binnacle/internal/config"
	"github.com/runlevel-six/binnacle/pkg/tui"
)

// Build metadata, injected via -ldflags. Source builds report "dev".
//
// The linker writes these, and [buildInfo] carries them to the rest of the
// program; the internal/build package documents why the -X flags name this
// package and not that one.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// buildInfo gathers the linker's metadata for everything that reports a version.
func buildInfo() build.Info {
	return build.Info{Version: version, Commit: commit, Date: date}
}

func main() { os.Exit(sextant()) }

// sextant runs the command and returns its exit status.
//
// Separate from main so that os.Exit cannot skip the deferred signal-handler
// cleanup: os.Exit runs no defers, so calling it inside main would leak the
// notification channel on every error path.
func sextant() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, context.Canceled) {
			return 0 // an interrupt is not a failure
		}
		fmt.Fprintln(os.Stderr, "sextant:", err)
		return 1
	}
	return 0
}

type options struct {
	cfg config.Config

	configPath    string
	serverURL     string
	serverCluster string
	token         string
	listContexts  bool
	listProfiles  bool
	signOut       bool
	listThemes    bool
	debugSnapshot bool
	debugDuration time.Duration
	verbose       bool
	showVersion   bool
	writeConfig   bool
	dryRun        bool
	demo          bool
	demoFleet     bool
	render        string
}

func run(ctx context.Context, args []string, out io.Writer) error {
	opts, err := parseFlags(args, out)
	if err != nil {
		return err
	}

	switch {
	case opts == nil:
		return nil // --help was handled during parsing
	case opts.showVersion:
		fmt.Fprintln(out, buildInfo().Line())
		return nil
	case opts.writeConfig:
		return writeExampleConfig(out, opts.configPath)
	}

	// Layer configuration: flags are already in place, then environment, then
	// the file underneath both.
	opts.cfg.MergeEnv()
	if err := opts.cfg.MergeFile(opts.configPath); err != nil {
		return err
	}

	if opts.listThemes {
		return app.ListThemes(out)
	}
	if opts.listProfiles {
		return app.ListProfiles(out)
	}
	if opts.listContexts {
		return app.ListContexts(out, opts.cfg)
	}

	// --server takes precedence over every other mode: it reads from a
	// binnacle deployment rather than a kubeconfig, so none of the cluster
	// resolution or debug flags apply. --profile does: the data comes from the
	// server, but the site conventions the panes are *rendered* with are
	// applied here, and nothing publishes a profile over the API.
	//
	// The flag, --token, and --server-cluster fall back to the config file's
	// server: section and the SEXTANT_SERVER* env vars, which MergeEnv and
	// MergeFile have already layered underneath the flags.
	serverURL := firstNonEmpty(opts.serverURL, opts.cfg.Server.URL)

	// Before the server branch: signing out must work against a server that is
	// unreachable, which is when it is most often wanted.
	if opts.signOut {
		if serverURL == "" {
			return errors.New("--sign-out needs a server: pass --server, or set one in the config file")
		}
		return app.SignOutServer(ctx, serverURL, out)
	}

	if serverURL != "" {
		cluster := firstNonEmpty(opts.serverCluster, opts.cfg.Server.Cluster)
		theme, err := tui.LookupTheme(opts.cfg.Theme)
		if err != nil {
			return err
		}
		// Before the credential, deliberately: signing in may open a browser
		// and wait for a human, and a misspelled profile should not cost them
		// that first.
		prof, err := app.SelectProfile(opts.cfg.Profile)
		if err != nil {
			return err
		}

		// Settle the credential before the UI takes the screen: signing in may
		// have to ask the operator something. An empty token is a valid
		// answer — a binnacle with no identity provider wants none.
		srv := opts.cfg.Server
		srv.Token = firstNonEmpty(opts.token, srv.Token)
		session, err := app.ResolveServerSession(ctx, serverURL, srv, os.Stderr)
		if err != nil {
			return err
		}

		return app.RunServer(ctx, app.ServerOptions{
			URL:     serverURL,
			Token:   session.Token,
			Cluster: cluster,
			Profile: prof,
			Theme:   theme,
			Build:   buildInfo(),
		})
	}

	// --demo-fleet is the fleet-screen counterpart of --demo: no server,
	// no kubeconfig, just fixture data.
	if opts.demoFleet {
		if opts.render != "" {
			w, h, err := parseSize(opts.render)
			if err != nil {
				return err
			}
			return app.RenderFleetDemo(ctx, opts.cfg, buildInfo(), out, w, h)
		}
		return app.RunFleetDemo(ctx, opts.cfg, buildInfo())
	}

	// The demo path resolves nothing against a kubeconfig, so it branches before
	// Prepare rather than inside it.
	if opts.demo {
		setup, err := app.PrepareDemo(opts.cfg, buildInfo())
		if err != nil {
			return err
		}
		if opts.render != "" {
			w, h, err := parseSize(opts.render)
			if err != nil {
				return err
			}
			return app.RenderDemo(ctx, setup, out, w, h)
		}
		return app.RunDemo(ctx, setup)
	}
	if opts.render != "" {
		return errors.New("--render only applies with --demo or --demo-fleet")
	}

	// An ambiguous context pattern asks, when someone is there to answer, and
	// errors with the flag to pin one when nobody is.
	setup, err := app.Prepare(opts.cfg, buildInfo(), app.InteractivePicker())
	if err != nil {
		return err
	}

	if opts.debugSnapshot {
		return app.DebugSnapshot(ctx, setup, out, opts.debugDuration, opts.verbose)
	}
	if opts.dryRun {
		fmt.Fprintf(out, "Resolved management context: %s\n", setup.Resolved.ManagementContext)
		fmt.Fprintf(out, "Resolved workload context:   %s\n", setup.Resolved.WorkloadContext)
		fmt.Fprintf(out, "Profile:                     %s\n", setup.Resolved.Profile.Name)
		fmt.Fprintf(out, "Theme:                       %s\n", setup.Resolved.Theme.Name)
		if name := setup.Resolved.CAPIClusterName; name != "" {
			fmt.Fprintf(out, "Cluster API cluster:         %s\n", name)
		}
		if cloud := setup.Resolved.OSCloud; cloud != "" {
			fmt.Fprintf(out, "OpenStack cloud:             %s\n", cloud)
		}
		return nil
	}
	return app.Run(ctx, setup)
}

// parseSize parses a WIDTHxHEIGHT terminal size for --render.
func parseSize(s string) (width, height int, err error) {
	if _, err := fmt.Sscanf(s, "%dx%d", &width, &height); err != nil {
		return 0, 0, fmt.Errorf("--render %q: want WIDTHxHEIGHT, e.g. 200x50", s)
	}
	return width, height, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseFlags returns nil options when --help was served, so the caller exits
// quietly rather than treating help as an error.
func parseFlags(args []string, out io.Writer) (*options, error) {
	var o options

	fs := flag.NewFlagSet("sextant", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.StringVar(&o.cfg.Management.Context, "management-context", "",
		"kubeconfig context for the management cluster (default: current context)")
	fs.StringVar(&o.cfg.Workload.Context, "workload-context", "",
		"kubeconfig context for the workload cluster (default: same as management)")
	fs.StringVar(&o.cfg.Profile, "profile", "", "site profile to apply")
	fs.StringVar(&o.cfg.TargetVersion, "target-version", "",
		"Kubernetes version being rolled out; activates rollout mode")
	fs.StringVar(&o.cfg.KubeconfigPath, "kubeconfig", "", "override $KUBECONFIG with a single file")
	fs.StringVar(&o.cfg.OSCloud, "os-cloud", "",
		"clouds.yaml profile for the OpenStack plugin (default: $OS_CLOUD, then the site profile)")
	fs.StringVar(&o.cfg.Theme, "theme", "",
		"color scheme; see --list-themes for the current set")
	fs.StringVar(&o.configPath, "config", config.DefaultPath(), "config file path")

	fs.BoolVar(&o.listContexts, "list-contexts", false,
		"list kubeconfig contexts and which sextant would select, then exit")
	fs.BoolVar(&o.listProfiles, "list-profiles", false,
		"list the profiles that can be loaded, then exit")
	fs.BoolVar(&o.signOut, "sign-out", false,
		"discard the saved credential for --server and revoke it at the provider, then exit")
	fs.BoolVar(&o.listThemes, "list-themes", false,
		"list the available color schemes, then exit")
	fs.BoolVar(&o.debugSnapshot, "debug-snapshot", false,
		"start the watchers, summarize every data source, then exit")
	fs.DurationVar(&o.debugDuration, "debug-duration", 10*time.Second,
		"how long --debug-snapshot waits for caches to warm")
	fs.BoolVar(&o.verbose, "v", false, "with --debug-snapshot, also print a sample item per source")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&o.writeConfig, "init", false, "write an example config file and exit")
	fs.BoolVar(&o.dryRun, "dry-run", false,
		"resolve configuration and print what would be watched, without starting the dashboard")
	fs.BoolVar(&o.demo, "demo", false,
		"run against invented data, with no kubeconfig and no cluster")
	fs.BoolVar(&o.demoFleet, "demo-fleet", false,
		"run the fleet screen against invented fleet data, with no server and no cluster")
	fs.StringVar(&o.render, "render", "",
		"with --demo or --demo-fleet, print one frame at WIDTHxHEIGHT and exit (e.g. 200x50)")
	fs.StringVar(&o.serverURL, "server", "",
		"connect to a binnacle server at this URL instead of reading a kubeconfig")
	fs.StringVar(&o.serverCluster, "server-cluster", "",
		"with --server, skip the fleet screen and go straight to this cluster (namespace/name)")
	fs.StringVar(&o.token, "token", "",
		"bearer token for --server; a server with --allow-unauthenticated does not need one")

	fs.Usage = func() { usage(out, fs) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

func usage(out io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(out, "sextant — terminal dashboard for Cluster API on bare metal")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  sextant [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "With no flags at all, sextant watches your kubeconfig's current context")
	fmt.Fprintln(out, "as both the management and the workload cluster.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	fs.PrintDefaults()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  # Verify sextant can see your cluster:")
	fmt.Fprintln(out, "  sextant --debug-snapshot -v")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # See what the context resolver sees:")
	fmt.Fprintln(out, "  sextant --list-contexts")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # Apply a site profile (a name, or a path to a YAML file):")
	fmt.Fprintln(out, "  sextant --profile openstack")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # Watch a specific OpenStack cloud, for an operator with several:")
	fmt.Fprintln(out, "  sextant --profile openstack --os-cloud my-cloud")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # Watch a management cluster and a separate workload cluster:")
	fmt.Fprintln(out, "  sextant --management-context mgmt --workload-context prod")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # Connect to a binnacle server instead of a kubeconfig:")
	fmt.Fprintln(out, "  sextant --server http://binnacle:8080")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # Connect and go straight to one cluster's detail:")
	fmt.Fprintln(out, "  sextant --server http://binnacle:8080 --server-cluster managed-clusters/tenant-03-cluster")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # Preview the fleet screen with fixture data:")
	fmt.Fprintln(out, "  sextant --demo-fleet")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # Boldly go:")
	fmt.Fprintln(out, "  sextant --theme lcars")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Environment: %s, %s, %s, %s, %s, %s, %s, %s, %s\n",
		config.EnvManagementContext, config.EnvWorkloadContext,
		config.EnvProfile, config.EnvTargetVersion, config.EnvTheme, config.EnvOSCloud,
		config.EnvServerURL, config.EnvServerCluster, config.EnvServerToken)
}

func writeExampleConfig(out io.Writer, path string) error {
	if path == "" {
		return errors.New("cannot determine a config path; pass --config")
	}
	if _, err := os.Stat(path); err == nil {
		// Overwriting would discard the user's settings, and this flag is easy
		// to run twice.
		return fmt.Errorf("%s already exists; remove it first or pass --config elsewhere", path)
	}
	if err := config.WriteExample(path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote an example config to %s\n", path)
	return nil
}
