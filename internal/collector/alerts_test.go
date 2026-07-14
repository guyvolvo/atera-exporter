package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/guyvolvo/atera-exporter/internal/atera"
)

func alertsHandler(alerts []map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":          alerts,
			"page":           1,
			"itemsInPage":    len(alerts),
			"totalItemCount": len(alerts),
			"totalPages":     1,
		})
	})
}

func alert(id int, severity, category, source string, archived bool, created time.Time) map[string]any {
	return map[string]any{
		"AlertID":         id,
		"Severity":        severity,
		"AlertCategoryID": category,
		"Source":          source,
		"Archived":        archived,
		"Created":         created.UTC().Format(time.RFC3339),
	}
}

func newTestAlerts(t *testing.T, srvURL string) *Alerts {
	t.Helper()
	client := atera.NewClient(atera.Options{
		BaseURL:           srvURL,
		APIKey:            "test",
		RequestsPerSecond: 1000,
		Burst:             1000,
	})
	return NewAlerts(client, time.Minute)
}

// Atera's /alerts returns the full history, most of it archived. Counting
// archived alerts as open would show a permanently red dashboard that never
// clears — the alert panel would be useless within a week.
func TestArchivedAlertsAreExcluded(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(alertsHandler([]map[string]any{
		alert(1, "Critical", "Availability", "DeviceMonitoring", true, now.Add(-72*time.Hour)),
		alert(2, "Critical", "Availability", "DeviceMonitoring", true, now.Add(-48*time.Hour)),
		alert(3, "Critical", "Availability", "DeviceMonitoring", false, now.Add(-time.Hour)),
		alert(4, "Warning", "Performance", "ThresholdCheck", false, now.Add(-30*time.Minute)),
	}))
	defer srv.Close()

	a := newTestAlerts(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	expected := `
# HELP atera_alerts Open (non-archived) alerts by severity, category and source.
# TYPE atera_alerts gauge
atera_alerts{category="Availability",severity="Critical",source="DeviceMonitoring"} 1
atera_alerts{category="Performance",severity="Warning",source="ThresholdCheck"} 1
`
	if err := testutil.CollectAndCompare(a, strings.NewReader(expected), "atera_alerts"); err != nil {
		t.Fatal(err)
	}
}

// The oldest open alert drives the "nobody is actioning this" alarm, so an
// archived alert must not be allowed to win the age comparison.
func TestOldestAlertIgnoresArchived(t *testing.T) {
	now := time.Now()
	ancientArchived := now.Add(-90 * 24 * time.Hour)
	oldestOpen := now.Add(-5 * 24 * time.Hour).Truncate(time.Second)

	srv := httptest.NewServer(alertsHandler([]map[string]any{
		alert(1, "Critical", "Availability", "Monitoring", true, ancientArchived),
		alert(2, "Critical", "Availability", "Monitoring", false, oldestOpen),
		alert(3, "Critical", "Availability", "Monitoring", false, now.Add(-time.Hour)),
	}))
	defer srv.Close()

	a := newTestAlerts(t, srv.URL)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got := gaugeValue(t, a, "atera_oldest_alert_timestamp_seconds")
	if int64(got) != oldestOpen.Unix() {
		t.Fatalf("oldest open alert = %d, want %d (an archived alert won the comparison)",
			int64(got), oldestOpen.Unix())
	}
}
