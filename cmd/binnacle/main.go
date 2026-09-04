// Command binnacle serves a fleet view of Cluster API clusters in a browser.
//
// It is the web front end to sextant: the same collectors, the same verdicts,
// laid out for a team that would rather open a tab than a terminal. Binnacle
// discovers what to watch from the management cluster's own Cluster objects, so
// there is no cluster list to maintain and nothing to go stale.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/runlevel-six/binnacle/pkg/profile"

	"github.com/runlevel-six/binnacle/internal/auth"
	"github.com/runlevel-six/binnacle/internal/fleet"
	"github.com/runlevel-six/binnacle/internal/web"
)

// version is set at build time by the release tooling.
var version = "dev"

type options struct {
	addr           string
	kubeconfig     string
	context        string
	namespace      string
	mgmtName       string
	profileName    string
	site           string
	demo           bool
	osCloud        string
	cloudsDir      string
	oidcIssuer     string
	oidcClientID   string
	oidcCLIClient  string
	oidcRedirect   string
	oidcCLIScopes  string
	insecureCookie bool
	allowUnauth    bool
	scopeFile      string
	showVersion    bool
}

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "binnacle:", err)
		os.Exit(1)
	}
}

// run takes its output writer rather than reaching for os.Stdout, so that the
// version line is testable — the same shape sextant's run has. Flag parsing
// errors and --help stay on the flag set's own output, which is stderr: a
// usage error is not output.
func run(args []string, out io.Writer) error {
	var o options
	fs := flag.NewFlagSet("binnacle", flag.ContinueOnError)
	fs.StringVar(&o.addr, "addr", "127.0.0.1:8080", "address to listen on")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to a kubeconfig; empty uses in-cluster credentials, then $KUBECONFIG")
	fs.StringVar(&o.context, "management-context", "", "kubeconfig context for the management cluster")
	fs.StringVar(&o.namespace, "namespace", "", "namespace to discover clusters in; empty means all")
	fs.StringVar(&o.mgmtName, "management-name", "",
		"what this management cluster is called locally, e.g. admin-k8s00; empty renders it as \"Management cluster\"")
	fs.StringVar(&o.profileName, "profile", "", "sextant site profile describing how these clusters are laid out")
	fs.StringVar(&o.site, "site", "", "the site or datacenter this instance watches, shown in the header and browser title; a label, not the --profile and not --management-name")
	fs.StringVar(&o.osCloud, "os-cloud", "", "clouds.yaml entry to use for clusters whose own credentials do not name one")
	fs.StringVar(&o.cloudsDir, "clouds-dir", "", "where per-cluster clouds.yaml files are written; empty uses a directory under the system temp dir")
	fs.StringVar(&o.oidcIssuer, "oidc-issuer", "", "OpenID Connect issuer URL, e.g. a Keycloak realm")
	fs.StringVar(&o.oidcClientID, "oidc-client-id", "", "OpenID Connect client id")
	fs.StringVar(&o.oidcCLIClient, "oidc-cli-client-id", "",
		"OpenID Connect client id for terminal clients, whose tokens are also accepted; empty uses --oidc-client-id")
	fs.StringVar(&o.oidcRedirect, "oidc-redirect-url", "", "binnacle's callback URL as the browser reaches it")
	fs.StringVar(&o.oidcCLIScopes, "oidc-cli-scopes", "",
		"comma-separated scopes a terminal client should request; empty uses the browser's. "+
			"Add offline_access so a terminal session survives the provider's SSO idle timeout")
	fs.BoolVar(&o.demo, "demo", false, "serve an invented fleet instead of a real one; needs no cluster and no credentials")
	fs.BoolVar(&o.allowUnauth, "allow-unauthenticated", false,
		"serve without authentication on a non-loopback address; every reader sees every cluster binnacle can read")
	fs.BoolVar(&o.insecureCookie, "insecure-cookies", false, "send session cookies without the Secure flag; for testing over plain HTTP only")
	fs.StringVar(&o.scopeFile, "scope-file", "", "path to a YAML file mapping OIDC groups to namespaces; empty means no scoping (everyone sees everything)")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "binnacle serves a fleet view of Cluster API clusters.")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "The OIDC client secret is read from $BINNACLE_OIDC_CLIENT_SECRET, and the")
		fmt.Fprintln(fs.Output(), "session signing key from $BINNACLE_SESSION_KEY (any secret of 32+ characters,")
		fmt.Fprintln(fs.Output(), "or base64 of 32+ bytes). Neither is a flag: a command line is visible in")
		fmt.Fprintln(fs.Output(), "the process table.")
		fmt.Fprintln(fs.Output())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Before anything that needs a cluster or a credential. The release ships
	// this as a downloadable binary, and the first question somebody has about
	// a binary they just downloaded is which one it is — a version that only
	// appears in a page footer, after the server has found a kubeconfig and an
	// identity provider, does not answer it.
	if o.showVersion {
		fmt.Fprintln(out, version)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	authenticator, err := buildAuth(ctx, o)
	if err != nil {
		return err
	}
	if _, open := authenticator.(auth.Open); open {
		if err := auth.RequireOIDCOffLoopback(o.addr, false, o.allowUnauth); err != nil {
			return err
		}
		if o.allowUnauth {
			// Swapped for the scheme that says so on the page. Setting the flag
			// is a decision by whoever ran the process; everyone who opens the
			// page afterwards is entitled to know it was made.
			authenticator = auth.Unauthenticated{}
			log.Println("warning: serving with no authentication. Anyone who can reach " +
				o.addr + " sees every cluster binnacle can read.")
		}
	}

	source, describe, err := buildSource(ctx, o)
	if err != nil {
		return err
	}

	groupScopes, err := web.LoadGroupScopes(o.scopeFile)
	if err != nil {
		return err
	}

	srv, err := web.New(source, authenticator, version, o.site, groupScopes)
	if err != nil {
		return err
	}
	site := o.site
	if site == "" {
		site = "unnamed"
	}
	log.Printf("binnacle %s watching %s (%s), listening on %s (%s)",
		version, site, describe, o.addr, authenticator.Describe())
	return srv.ServeContext(ctx, o.addr)
}

// buildSource returns what the page renders and a phrase describing it.
//
// The demo path deliberately touches no cluster machinery at all: it does not
// load a profile, resolve credentials, or contact anything. Someone looking at
// the layout should not need a kubeconfig, and a demo that can fail to start
// for cluster reasons is not much of a demo.
func buildSource(ctx context.Context, o options) (web.Source, string, error) {
	if o.demo {
		d := fleet.NewDemo()
		go d.Run(ctx)
		return d, "an invented fleet", nil
	}

	mgmt, err := managementConfig(o)
	if err != nil {
		return nil, "", err
	}
	prof, err := profile.NewLoader().Load(o.profileName)
	if err != nil {
		return nil, "", fmt.Errorf("load profile: %w", err)
	}

	f, err := fleet.New(fleet.Options{
		Management:     mgmt,
		ManagementName: o.mgmtName,
		Namespace:      o.namespace,
		Profile:        prof,
		OSCloud:        o.osCloud,
		CloudsDir:      o.cloudsDir,
	})
	if err != nil {
		return nil, "", err
	}
	go func() {
		if err := f.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("cluster discovery stopped: %v", err)
		}
	}()
	return f, fmt.Sprintf("profile %q", prof.Name), nil
}

// buildAuth picks the scheme from what was configured.
//
// Naming an issuer is what turns authentication on. There is no --auth flag,
// because a flag whose safe value is the non-default one is a mistake waiting
// for a hurried deployment; the guard in [auth.RequireOIDCOffLoopback] catches
// the case where nothing was configured and the listener is not local.
func buildAuth(ctx context.Context, o options) (web.Authenticator, error) {
	if o.oidcIssuer == "" {
		return auth.Open{}, nil
	}

	key, err := sessionKey()
	if err != nil {
		return nil, err
	}
	return auth.NewOIDC(ctx, auth.OIDCConfig{
		Issuer:       o.oidcIssuer,
		ClientID:     o.oidcClientID,
		CLIClientID:  o.oidcCLIClient,
		ClientSecret: os.Getenv("BINNACLE_OIDC_CLIENT_SECRET"),
		RedirectURL:  o.oidcRedirect,
		CLIScopes:    splitScopes(o.oidcCLIScopes),
		SessionKey:   key,
		Secure:       !o.insecureCookie,
	})
}

// splitScopes parses a comma-separated scope list, ignoring empties so that a
// trailing comma or a stray space is not published to every client as a scope
// named "".
func splitScopes(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sessionKey reads the cookie signing key, generating one if none was given.
//
// The value may be base64 or it may be the key itself. Base64 is what the
// documented one-liner produces and what gets tried first, but a signing key is
// opaque bytes, and rejecting a perfectly good random secret because it happens
// not to be valid base64 is an implementation detail in an operator's way. The
// error that produced — "illegal base64 data at input byte 0" — said nothing
// about what to do next.
//
// A generated key works for a single replica and logs why it is not enough for
// two: sessions signed by one pod are rejected by the other, and the symptom is
// a login that loops rather than an error anyone can read.
func sessionKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv("BINNACLE_SESSION_KEY"))
	if raw == "" {
		key, err := auth.NewSessionKey()
		if err != nil {
			return nil, err
		}
		log.Println("warning: BINNACLE_SESSION_KEY is unset, so a random key was generated. " +
			"Sessions will not survive a restart, and with more than one replica sign-in will loop.")
		return key, nil
	}

	// Decoded base64, when that is what it is and it carries enough entropy.
	// The length check matters: a 32-character secret that happens to be valid
	// base64 decodes to 24 bytes, and the raw form is the better key.
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil && len(key) >= minSessionKey {
		return key, nil
	}
	if len(raw) < minSessionKey {
		return nil, fmt.Errorf(
			"BINNACLE_SESSION_KEY is %d characters; want at least %d, or base64 of at least %d bytes",
			len(raw), minSessionKey, minSessionKey)
	}
	return []byte(raw), nil
}

// minSessionKey is the shortest key accepted, in bytes. It is the size of the
// HMAC-SHA256 block the key feeds, below which the extra length buys nothing
// and above which a shorter secret is simply a weaker one.
const minSessionKey = 32

// managementConfig resolves credentials for the management cluster.
//
// In-cluster credentials win when they exist, because that is the deployed case
// and it needs no configuration at all. A kubeconfig is the developer's path.
func managementConfig(o options) (*rest.Config, error) {
	if o.kubeconfig == "" && o.context == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.kubeconfig != "" {
		rules.ExplicitPath = o.kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: o.context},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("management cluster credentials: %w", err)
	}
	return cfg, nil
}
