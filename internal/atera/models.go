package atera

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Time wraps time.Time because Atera is inconsistent about timestamp formats:
// some endpoints return RFC3339 with an offset, others return a bare local
// datetime with fractional seconds and no zone. A strict time.Time unmarshal
// fails on the latter.
type Time struct {
	time.Time
}

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.9999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed
			return nil
		}
	}
	// An unparseable timestamp must not fail the whole collection cycle.
	return nil
}

// page is the envelope every paginated Atera list endpoint returns.
type page[T any] struct {
	Items          []T    `json:"items"`
	TotalItemCount int    `json:"totalItemCount"`
	Page           int    `json:"page"`
	ItemsInPage    int    `json:"itemsInPage"`
	TotalPages     int    `json:"totalPages"`
	NextLink       string `json:"nextLink"`
}

// MiB converts Atera's size fields to bytes. Atera reports Memory and
// HardwareDisks in mebibytes (a 512GB SSD reports Total: 487058), while
// Prometheus convention requires base units.
const MiB = 1024 * 1024

type Agent struct {
	AgentID      int      `json:"AgentID"`
	DeviceGUID   string   `json:"DeviceGuid"`
	AgentName    string   `json:"AgentName"`
	MachineName  string   `json:"MachineName"`
	FolderID     int      `json:"FolderID"`
	FolderName   string   `json:"FolderName"`
	CustomerID   int      `json:"CustomerID"`
	CustomerName string   `json:"CustomerName"`
	Online       bool     `json:"Online"`
	DeviceType   string   `json:"DeviceType"`
	OS           string   `json:"OS"`
	OSType       string   `json:"OSType"`
	OSVersion    string   `json:"OSVersion"`
	OSBuild      string   `json:"OSBuild"`
	AgentVersion string   `json:"AgentVersion"`
	DomainName   string   `json:"DomainName"`
	Vendor       string   `json:"Vendor"`
	Model        string   `json:"VendorBrandModel"`
	IPAddresses  []string `json:"IpAddresses"`
	LastSeen     Time     `json:"LastSeen"`
	LastReboot   Time     `json:"LastRebootTime"`
	MemoryMiB    float64  `json:"Memory"`
	Disks        []Disk   `json:"HardwareDisks"`
}

// ID is the stable identity label. v3 has no MachineID; AgentID is the integer
// primary key and is what every other v3 endpoint keys off.
func (a Agent) ID() string {
	return strconv.Itoa(a.AgentID)
}

func (a Agent) Hostname() string {
	if a.MachineName != "" {
		return clean(a.MachineName)
	}
	return clean(a.AgentName)
}

// OSName is the human OS string. Do not use OSType for this — it holds the device
// class ("Work Station"), not the operating system.
func (a Agent) OSName() string {
	return clean(a.OS)
}

// Folder names the grouping bucket. A third of this estate's agents have no
// folder set; Prometheus treats an empty label value as absent, which would drop
// them out of `sum by (folder)` rather than showing them as a group.
func (a Agent) Folder() string {
	if f := clean(a.FolderName); f != "" {
		return f
	}
	return "unassigned"
}

func (a Agent) IP() string {
	if len(a.IPAddresses) == 0 {
		return ""
	}
	return a.IPAddresses[0]
}

func (a Agent) MemoryBytes() float64 {
	return a.MemoryMiB * MiB
}

type Disk struct {
	Drive    string  `json:"Drive"`
	TotalMiB float64 `json:"Total"`
	FreeMiB  float64 `json:"Free"`
	UsedMiB  float64 `json:"Used"`
}

func (d Disk) TotalBytes() float64 { return d.TotalMiB * MiB }
func (d Disk) FreeBytes() float64  { return d.FreeMiB * MiB }

// clean strips formatting characters and collapses whitespace. Hebrew-locale Atera
// tenants prefix strings such as OS with bidi marks (LRM, RLM). They render as
// nothing, so a label carrying them looks correct on screen while failing every
// equality match in PromQL.
//
// unicode.Cf is the "format" category: it covers the bidi marks, embeddings and
// isolates, and the BOM, without hardcoding a list of code points.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

type Alert struct {
	AlertID    int    `json:"AlertID"`
	Severity   string `json:"Severity"`
	Title      string `json:"Title"`
	Source     string `json:"Source"`
	Category   string `json:"AlertCategoryID"`
	Code       int    `json:"Code"`
	Archived   bool   `json:"Archived"`
	DeviceGUID string `json:"DeviceGuid"`
	DeviceName string `json:"DeviceName"`
	Created    Time   `json:"Created"`
}

type Ticket struct {
	TicketID        int    `json:"TicketID"`
	TicketTitle     string `json:"TicketTitle"`
	TicketStatus    string `json:"TicketStatus"`
	TicketPriority  string `json:"TicketPriority"`
	TicketType      string `json:"TicketType"`
	TicketImpact    string `json:"TicketImpact"`
	TicketSource    string `json:"TicketSource"`
	TechnicianName  string `json:"TechnicianFullName"`
	TechnicianEmail string `json:"TechnicianEmail"`
	Created         Time   `json:"TicketCreatedDate"`
	Resolved        Time   `json:"TicketResolvedDate"`
}

// Technician labels unassigned work explicitly rather than emitting an empty
// label, which would be indistinguishable from a scrape gap on a dashboard.
func (t Ticket) Technician() string {
	name := clean(t.TechnicianName)
	if name == "" || name == "." {
		return "unassigned"
	}
	return name
}

type Contract struct {
	ContractID   int    `json:"ContractID"`
	ContractName string `json:"ContractName"`
	ContractType string `json:"ContractType"`
	CustomerID   int    `json:"CustomerID"`
	CustomerName string `json:"CustomerName"`
	Active       bool   `json:"Active"`
	EndDate      Time   `json:"EndDate"`
}

// Raw is used by dump mode to print a page of any endpoint without a model.
type Raw = json.RawMessage
