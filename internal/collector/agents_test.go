package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/guyvolvo/atera-exporter/internal/atera"
)

// fakeAtera serves a paginated /agents endpoint from a fleet the test controls,
// so pagination and snapshot replacement are exercised for real rather than
// asserted.
type fakeAtera struct {
	agents []map[string]any
	calls  int
}

func (f *fakeAtera) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++

		const perPage = 2
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}

		total := len(f.agents)
		totalPages := (total + perPage - 1) / perPage

		start := (page - 1) * perPage
		end := min(start+perPage, total)
		var items []map[string]any
		if start < total {
			items = f.agents[start:end]
		}

		json.NewEncoder(w).Encode(map[string]any{
			"items":          items,
			"page":           page,
			"itemsInPage":    len(items),
			"totalItemCount": total,
			"totalPages":     totalPages,
		})
	})
}

// Mirrors the real v3 /agents payload: AgentID is an integer, there is no
// MachineID, and sizes are in mebibytes.
func agent(id int, name, folder string, online bool) map[string]any {
	return map[string]any{
		"AgentID":      id,
		"DeviceGuid":   "guid-" + strconv.Itoa(id),
		"MachineName":  name,
		"FolderName":   folder,
		"CustomerName": "Epstein",
		"Online":       online,
		"OS":           "Microsoft Windows 11 Pro x64",
		"OSType":       "Work Station",
		"DeviceType":   "PC",
	}
}

func newTestAgents(t *testing.T, srvURL string) *Agents {
	t.Helper()
	client := atera.NewClient(atera.Options{
		BaseURL:           srvURL,
		APIKey:            "test",
		RequestsPerSecond: 1000,
		Burst:             1000,
	})
	return NewAgents(client, time.Minute)
}

// Five agents at two per page must produce five agents, not the first two.
// This is the bug that makes a naive exporter silently monitor a fraction of the
// fleet.
func TestRefreshWalksAllPages(t *testing.T) {
	fake := &fakeAtera{agents: []map[string]any{
		agent(1, "PC1", "Epstein-Computers", true),
		agent(2, "PC2", "Epstein-Computers", true),
		agent(3, "PC3", "Epstein-Computers", false),
		agent(4, "PC4", "Epstein-Servers", true),
		agent(5, "PC5", "Epstein-Servers", false),
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	a := newTestAgents(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := testutil.CollectAndCount(a, "atera_agent_online"); got != 5 {
		t.Fatalf("atera_agent_online series = %d, want 5 (pagination is broken)", got)
	}
	if fake.calls != 3 {
		t.Fatalf("http calls = %d, want 3 pages", fake.calls)
	}
}

// A device removed from Atera must stop producing series. Holding a GaugeVec
// instead of an immutable snapshot would leave m3 reporting online=0 forever and
// corrupt the denominator of every percent-online query.
func TestDeletedAgentDisappears(t *testing.T) {
	fake := &fakeAtera{agents: []map[string]any{
		agent(1, "PC1", "Epstein-Computers", true),
		agent(2, "PC2", "Epstein-Computers", true),
		agent(3, "PC3", "Epstein-Computers", false),
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	a := newTestAgents(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := testutil.CollectAndCount(a, "atera_agent_online"); got != 3 {
		t.Fatalf("before decommission: %d series, want 3", got)
	}

	fake.agents = fake.agents[:2] // m3 is decommissioned
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := testutil.CollectAndCount(a, "atera_agent_online"); got != 2 {
		t.Fatalf("after decommission: %d series, want 2 (stale series retained)", got)
	}

	expected := `
# HELP atera_agents_total Agent count per folder and online state. Cheap aggregate for dashboards.
# TYPE atera_agents_total gauge
atera_agents_total{folder="Epstein-Computers",online="false"} 0
atera_agents_total{folder="Epstein-Computers",online="true"} 2
`
	if err := testutil.CollectAndCompare(a, strings.NewReader(expected), "atera_agents_total"); err != nil {
		t.Fatal(err)
	}
}

// Atera returning the same MachineID twice must not blank the whole /metrics
// endpoint, which is what an unguarded duplicate label set does.
func TestDuplicateMachineIDDoesNotBreakScrape(t *testing.T) {
	fake := &fakeAtera{agents: []map[string]any{
		agent(1, "PC1", "Epstein-Computers", true),
		agent(1, "PC1-dup", "Epstein-Computers", true),
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	a := newTestAgents(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather failed on duplicate MachineID: %v", err)
	}
}

// Atera reports Memory and HardwareDisks in mebibytes. Publishing those numbers
// raw under a _bytes name would be wrong by a factor of ~1e6 and nothing would
// complain — the graph would just quietly lie.
func TestSizesAreConvertedToBytes(t *testing.T) {
	ag := agent(1, "PC1", "Epstein-Computers", true)
	ag["Memory"] = 32411
	ag["HardwareDisks"] = []map[string]any{
		{"Drive": "C:", "Total": 487058, "Free": 259806, "Used": 227252},
	}

	fake := &fakeAtera{agents: []map[string]any{ag}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	a := newTestAgents(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	const mib = 1024 * 1024
	expected := fmt.Sprintf(`
# HELP atera_agent_disk_free_bytes Free space on a device volume.
# TYPE atera_agent_disk_free_bytes gauge
atera_agent_disk_free_bytes{agent_id="1",drive="C:"} %d
# HELP atera_agent_disk_total_bytes Total capacity of a device volume.
# TYPE atera_agent_disk_total_bytes gauge
atera_agent_disk_total_bytes{agent_id="1",drive="C:"} %d
# HELP atera_agent_memory_bytes Installed physical memory.
# TYPE atera_agent_memory_bytes gauge
atera_agent_memory_bytes{agent_id="1"} %d
`, 259806*mib, 487058*mib, 32411*mib)

	if err := testutil.CollectAndCompare(a, strings.NewReader(expected),
		"atera_agent_disk_free_bytes", "atera_agent_disk_total_bytes", "atera_agent_memory_bytes"); err != nil {
		t.Fatal(err)
	}
}

// Atera lists the same volume two or three times in HardwareDisks for some
// agents. A duplicate label set makes Prometheus reject the whole scrape with a
// 500 — not just that series — so /metrics returns nothing at all.
func TestDuplicateDrivesDoNotBreakScrape(t *testing.T) {
	ag := agent(2089, "PC-2089", "Epstein-Computers", true)
	ag["HardwareDisks"] = []map[string]any{
		{"Drive": "C:", "Total": 242928, "Free": 71324},
		{"Drive": "C:", "Total": 242928, "Free": 71324},
		{"Drive": "C:", "Total": 242928, "Free": 71324},
	}

	fake := &fakeAtera{agents: []map[string]any{ag}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	a := newTestAgents(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	reg := prometheus.NewRegistry()
	if err := reg.Register(a); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather failed on duplicate drive: %v", err)
	}

	if got := testutil.CollectAndCount(a, "atera_agent_disk_total_bytes"); got != 1 {
		t.Fatalf("disk series = %d, want 1 (duplicates not collapsed)", got)
	}
}

// Hebrew-locale tenants return OS strings padded with bidi control marks. Those
// are invisible, so a label carrying them looks correct on screen while failing
// every equality match in PromQL.
func TestBidiMarksStrippedFromLabels(t *testing.T) {
	ag := agent(1, "PC1", "Epstein-Computers", true)
	ag["OS"] = "‏‏Microsoft Windows 11 Pro  x64"

	fake := &fakeAtera{agents: []map[string]any{ag}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	a := newTestAgents(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP atera_agent_info Device metadata. Always 1; join against this for labels.
# TYPE atera_agent_info gauge
atera_agent_info{agent_id="1",agent_version="",customer="Epstein",device_guid="guid-1",device_type="PC",folder="Epstein-Computers",hostname="PC1",ip="",model="",os="Microsoft Windows 11 Pro x64",os_version="",vendor=""} 1
`
	if err := testutil.CollectAndCompare(a, strings.NewReader(expected), "atera_agent_info"); err != nil {
		t.Fatal(err)
	}
}

// A failing poll must leave the last good snapshot in place. Zeroing the fleet
// because Atera had a bad minute would page the on-call for nothing.
func TestFailedRefreshKeepsPreviousSnapshot(t *testing.T) {
	fake := &fakeAtera{agents: []map[string]any{
		agent(1, "PC1", "Epstein-Computers", true),
		agent(2, "PC2", "Epstein-Computers", true),
	}}

	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fake.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	a := newTestAgents(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	fail = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.Refresh(ctx); err == nil {
		t.Fatal("expected refresh to fail on 500")
	}

	if got := testutil.CollectAndCount(a, "atera_agent_online"); got != 2 {
		t.Fatalf("after failed poll: %d series, want 2 retained", got)
	}
}
