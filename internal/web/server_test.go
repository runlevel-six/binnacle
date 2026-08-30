package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runlevel-six/sextant/pkg/health"
	"github.com/runlevel-six/sextant/pkg/model"

	"github.com/runlevel-six/binnacle/internal/auth"
	"github.com/runlevel-six/binnacle/internal/fleet"
)

type fakeFleet struct {
	clusters []fleet.ClusterView
	changed  chan struct{}
}

func (f *fakeFleet) View() []fleet.ClusterView { return f.clusters }
func (f *fakeFleet) Changed() <-chan struct{}  { return f.changed }

func serve(t *testing.T, clusters ...fleet.ClusterView) http.Handler {
	t.Helper()
	s, err := New(&fakeFleet{clusters: clusters, changed: make(chan struct{}, 1)}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestFleetPage_RendersClusters(t *testing.T) {
	h := serve(t, fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01", Version: "v1.31.4", Phase: "Provisioned",
		Status:       health.StatusOK,
		Cells:        []health.Cell{{Name: "Nodes", Status: health.StatusOK}},
		ControlPlane: model.ReplicaBucket{Desired: 3, Ready: 3},
		Workers:      model.ReplicaBucket{Desired: 5, Ready: 5},
		Nodes:        fleet.NodeCount{Ready: 8, Total: 8},
		UpdatedAt:    time.Now(),
	})
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"tenant-01", "capi", "v1.31.4", "3/3", "5/5", "8/8", "Nodes"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not mention %q", want)
		}
	}
}

// The severity has to reach the markup as something a stylesheet can key on.
// It is the only channel by which sextant's verdict becomes visible.
func TestFleetPage_StatusReachesTheMarkup(t *testing.T) {
	h := serve(t, fleet.ClusterView{Name: "tenant-01", Status: health.StatusWarn})
	if body := get(t, h, "/").Body.String(); !strings.Contains(body, `class="card warn"`) {
		t.Error("the card carries no severity class")
	}
}

// A row that cannot be read shows why, and shows no numbers at all. Counts
// under a connection error look current and are not.
func TestFleetPage_ProblemRowShowsNoCounts(t *testing.T) {
	h := serve(t, fleet.ClusterView{
		Namespace: "capi", Name: "tenant-01",
		Status:  health.StatusErr,
		Problem: "read tenant-01-kubeconfig: secret not found",
	})
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, "secret not found") {
		t.Error("the reason is not on the page")
	}
	for _, unwanted := range []string{"Control plane", "Workers", "version unknown", `<div class="seen">`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("an unreadable row still rendered %q", unwanted)
		}
	}
}

// An empty fleet says so rather than rendering a blank page that looks like a
// still-loading one.
func TestFleetPage_EmptyFleetSaysSo(t *testing.T) {
	if body := get(t, serve(t), "/").Body.String(); !strings.Contains(body, "No Cluster API clusters found") {
		t.Error("an empty fleet rendered nothing to explain itself")
	}
}

// A probe has no session, so gating it would make the deployment
// unschedulable.
func TestHealthz_IsNotGated(t *testing.T) {
	s, err := New(&fakeFleet{changed: make(chan struct{})}, auth.Open{}, "test", "site-a")
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(t, s.Handler(), "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("got %d", rec.Code)
	}
}

// The stream opens with the current state instead of waiting for the next
// change. A browser reconnecting after a blip would otherwise sit on stale
// markup until something in the fleet happened to move.
func TestEvents_FirstFrameIsImmediate(t *testing.T) {
	h := serve(t, fleet.ClusterView{Namespace: "capi", Name: "tenant-01", Status: health.StatusOK})

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	ctx, cancel := contextWithFrame(t)
	defer cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: fleet\n") {
		t.Fatalf("stream did not open with a fleet event:\n%s", firstLine(body))
	}
	if !strings.Contains(body, "tenant-01") {
		t.Error("first frame carries no cluster")
	}
	// Every line of the fragment needs its own data: prefix or the browser
	// silently drops the rest of the frame.
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" || strings.HasPrefix(line, "event: ") || strings.HasPrefix(line, "data: ") {
			continue
		}
		t.Errorf("unprefixed line in the SSE frame: %q", line)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestAgo(t *testing.T) {
	ago := funcs()["ago"].(func(time.Time) string)
	for name, tc := range map[string]struct {
		in   time.Time
		want string
	}{
		"zero":    {time.Time{}, "never"},
		"seconds": {time.Now().Add(-30 * time.Second), "30s ago"},
		"minutes": {time.Now().Add(-5 * time.Minute), "5m ago"},
		"hours":   {time.Now().Add(-3 * time.Hour), "3h ago"},
	} {
		if got := ago(tc.in); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

// One binnacle runs per management cluster and sites reuse workload cluster
// names, so several deployments render pages identical down to the names on the
// cards. The site has to reach both the header and the browser title, because a
// tab strip shows only the title.
func TestFleetPage_SiteNamesTheDeployment(t *testing.T) {
	body := get(t, serve(t, fleet.ClusterView{Name: "tenant-01"}), "/").Body.String()
	if !strings.Contains(body, "<title>site-a &middot; Binnacle</title>") {
		t.Error("the site is not in the browser title")
	}
	if !strings.Contains(body, `<span class="site">site-a</span>`) {
		t.Error("the site is not in the page header")
	}
}

// A single local instance has nothing to be confused with, so an unset site is
// absence rather than an empty badge.
func TestFleetPage_UnnamedSiteRendersNothingExtra(t *testing.T) {
	s, err := New(&fakeFleet{changed: make(chan struct{}, 1)}, auth.Open{}, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, s.Handler(), "/").Body.String()
	if !strings.Contains(body, "<title>Binnacle</title>") {
		t.Error("an unnamed instance should title plainly")
	}
	if strings.Contains(body, `class="site"`) {
		t.Error("an unnamed instance rendered an empty site badge")
	}
}

// A cluster that has not reported yet must not render zeros. They are numbers
// nobody has observed, and 0/0 in a replica column looks like a cluster with
// nothing in it rather than one we have not heard from — the milder form of
// the same mistake as showing counts under a connection error.
func TestFleetPage_UnreportedClusterShowsNoZeros(t *testing.T) {
	h := serve(t, fleet.ClusterView{
		Namespace: "capi", Name: "tenant-05",
		Status: health.StatusLoading,
		Cells:  []health.Cell{{Name: "CAPI", Status: health.StatusLoading}},
		// UpdatedAt deliberately zero: nothing has published.
	})
	body := get(t, h, "/").Body.String()
	if !strings.Contains(body, "Waiting for the first report") {
		t.Error("an unreported cluster does not say so")
	}
	for _, unwanted := range []string{"0/0", "Control plane", `<div class="seen">`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("an unreported cluster rendered %q", unwanted)
		}
	}
	// Its cells still belong to it: "CAPI, still loading" is real information.
	if !strings.Contains(body, "CAPI") {
		t.Error("cells were dropped along with the counts")
	}
}
