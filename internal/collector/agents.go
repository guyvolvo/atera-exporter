package collector

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/guyvolvo/atera-exporter/internal/atera"
)

// Agents exposes fleet inventory and online state.
//
// Metrics are emitted from an immutable snapshot rather than held in a GaugeVec.
// A GaugeVec would retain series for devices that no longer exist in Atera, so a
// decommissioned machine would report online=0 forever and quietly inflate the
// denominator of every "percent online" query. Replacing the whole snapshot each
// cycle makes deletions disappear for free.
//
// Aggregates are grouped by folder, not customer. On a single-tenant estate every
// agent carries the same CustomerName, so a per-customer breakdown collapses to one
// degenerate bucket; FolderName is the grouping that means anything. MSPs can group
// by the customer label on atera_agent_info instead.
type Agents struct {
	client   *atera.Client
	interval time.Duration
	snap     atomic.Pointer[[]atera.Agent]

	info       *prometheus.Desc
	online     *prometheus.Desc
	lastSeen   *prometheus.Desc
	lastReboot *prometheus.Desc
	total      *prometheus.Desc
	memory     *prometheus.Desc
	diskTotal  *prometheus.Desc
	diskFree   *prometheus.Desc
}

func NewAgents(client *atera.Client, interval time.Duration) *Agents {
	return &Agents{
		client:   client,
		interval: interval,

		// Descriptive labels live only on the info metric. Joining them onto the
		// numeric series with `* on(agent_id) group_left(...)` at query time keeps
		// hostname/OS churn from creating new series on every other metric.
		// device_guid is carried so a dashboard can deep-link into Atera.
		info: prometheus.NewDesc(
			"atera_agent_info",
			"Device metadata. Always 1; join against this for labels.",
			[]string{
				"agent_id", "device_guid", "hostname", "folder", "customer",
				"os", "os_version", "device_type", "agent_version", "vendor", "model", "ip",
			}, nil),

		online: prometheus.NewDesc(
			"atera_agent_online",
			"1 if Atera considers the agent online, 0 otherwise.",
			[]string{"agent_id", "folder"}, nil),

		lastSeen: prometheus.NewDesc(
			"atera_agent_last_seen_timestamp_seconds",
			"Unix time the agent last checked in.",
			[]string{"agent_id"}, nil),

		lastReboot: prometheus.NewDesc(
			"atera_agent_last_reboot_timestamp_seconds",
			"Unix time the device last rebooted. Subtract from time() for uptime.",
			[]string{"agent_id"}, nil),

		total: prometheus.NewDesc(
			"atera_agents_total",
			"Agent count per folder and online state. Cheap aggregate for dashboards.",
			[]string{"folder", "online"}, nil),

		memory: prometheus.NewDesc(
			"atera_agent_memory_bytes",
			"Installed physical memory.",
			[]string{"agent_id"}, nil),

		diskTotal: prometheus.NewDesc(
			"atera_agent_disk_total_bytes",
			"Total capacity of a device volume.",
			[]string{"agent_id", "drive"}, nil),

		diskFree: prometheus.NewDesc(
			"atera_agent_disk_free_bytes",
			"Free space on a device volume.",
			[]string{"agent_id", "drive"}, nil),
	}
}

func (a *Agents) Name() string            { return "agents" }
func (a *Agents) Interval() time.Duration { return a.interval }

func (a *Agents) Refresh(ctx context.Context) error {
	agents, err := a.client.Agents(ctx)
	if err != nil {
		return err
	}

	// Prometheus rejects the entire scrape on any duplicate label set - a single
	// duplicate blanks /metrics with a 500 rather than degrading one series. Atera
	// duplicates both of these in practice: repeated AgentIDs, and the same volume
	// listed two or three times in HardwareDisks.
	seen := make(map[string]struct{}, len(agents))
	unique := agents[:0]
	for _, ag := range agents {
		id := ag.ID()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ag.Disks = dedupeDisks(ag.Disks)
		unique = append(unique, ag)
	}

	a.snap.Store(&unique)
	return nil
}

// dedupeDisks keeps the first entry per drive letter. Atera repeats volumes in
// HardwareDisks with identical values, so the first is as good as any.
func dedupeDisks(disks []atera.Disk) []atera.Disk {
	if len(disks) < 2 {
		return disks
	}
	seen := make(map[string]struct{}, len(disks))
	out := disks[:0]
	for _, d := range disks {
		if d.Drive == "" {
			continue
		}
		if _, dup := seen[d.Drive]; dup {
			continue
		}
		seen[d.Drive] = struct{}{}
		out = append(out, d)
	}
	return out
}

func (a *Agents) Describe(ch chan<- *prometheus.Desc) {
	ch <- a.info
	ch <- a.online
	ch <- a.lastSeen
	ch <- a.lastReboot
	ch <- a.total
	ch <- a.memory
	ch <- a.diskTotal
	ch <- a.diskFree
}

func (a *Agents) Collect(ch chan<- prometheus.Metric) {
	snap := a.snap.Load()
	if snap == nil {
		return
	}

	type key struct{ folder, online string }
	counts := map[key]int{}
	folders := map[string]struct{}{}

	for _, ag := range *snap {
		id := ag.ID()

		onlineVal := 0.0
		onlineLabel := "false"
		if ag.Online {
			onlineVal = 1
			onlineLabel = "true"
		}
		counts[key{ag.Folder(), onlineLabel}]++
		folders[ag.Folder()] = struct{}{}

		ch <- prometheus.MustNewConstMetric(a.info, prometheus.GaugeValue, 1,
			id, ag.DeviceGUID, ag.Hostname(), ag.Folder(), ag.CustomerName,
			ag.OSName(), ag.OSVersion, ag.DeviceType, ag.AgentVersion,
			ag.Vendor, ag.Model, ag.IP())

		ch <- prometheus.MustNewConstMetric(a.online, prometheus.GaugeValue, onlineVal,
			id, ag.Folder())

		if !ag.LastSeen.IsZero() {
			ch <- prometheus.MustNewConstMetric(a.lastSeen, prometheus.GaugeValue,
				float64(ag.LastSeen.Unix()), id)
		}
		if !ag.LastReboot.IsZero() {
			ch <- prometheus.MustNewConstMetric(a.lastReboot, prometheus.GaugeValue,
				float64(ag.LastReboot.Unix()), id)
		}
		if ag.MemoryMiB > 0 {
			ch <- prometheus.MustNewConstMetric(a.memory, prometheus.GaugeValue,
				ag.MemoryBytes(), id)
		}

		for _, d := range ag.Disks {
			if d.Drive == "" {
				continue
			}
			ch <- prometheus.MustNewConstMetric(a.diskTotal, prometheus.GaugeValue, d.TotalBytes(), id, d.Drive)
			ch <- prometheus.MustNewConstMetric(a.diskFree, prometheus.GaugeValue, d.FreeBytes(), id, d.Drive)
		}
	}

	// Emit both online states for every folder even when one is zero, so a folder
	// going fully offline produces a 0 rather than a gap in the graph.
	for folder := range folders {
		for _, state := range []string{"true", "false"} {
			k := key{folder, state}
			if _, ok := counts[k]; !ok {
				counts[k] = 0
			}
		}
	}

	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(a.total, prometheus.GaugeValue, float64(n),
			k.folder, k.online)
	}
}
