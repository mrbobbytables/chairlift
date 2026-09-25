package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectbluefin/chairlift/internal/views/cleanupview"
	"github.com/projectbluefin/chairlift/internal/views/pageview"
)

const maintenanceATSPIWaitTimeout = 3 * time.Minute

// maintenanceRecord represents one tab-separated record emitted by the probe.
type maintenanceRecord struct {
	Kind   string
	Fields map[string]string
}

// parseMaintenanceATSPIRecords turns the probe's tab-separated records into
// structured records. Parsing is strict: missing keys, duplicate keys,
// trailing records after DONE, or missing DONE are errors.
func parseMaintenanceATSPIRecords(report string) ([]maintenanceRecord, error) {
	trimmed := strings.Trim(report, "\r\n")
	if trimmed == "" {
		return nil, fmt.Errorf("empty AT-SPI report")
	}

	var records []maintenanceRecord
	var done bool

	for lineNumber, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if done {
			return nil, fmt.Errorf("line %d: record %q follows DONE", lineNumber+1, line)
		}

		parts := strings.Split(line, "\t")
		if len(parts) == 0 || parts[0] == "" {
			return nil, fmt.Errorf("line %d: empty record kind", lineNumber+1)
		}

		kind := parts[0]
		if kind == "DONE" {
			if len(parts) > 1 {
				return nil, fmt.Errorf("line %d: DONE record has trailing fields", lineNumber+1)
			}
			done = true
			records = append(records, maintenanceRecord{Kind: "DONE", Fields: map[string]string{}})
			continue
		}

		fields := make(map[string]string, len(parts)-1)
		for _, part := range parts[1:] {
			key, value, found := strings.Cut(part, "=")
			if !found || key == "" {
				return nil, fmt.Errorf("line %d: field %q is not key=value", lineNumber+1, part)
			}
			if _, duplicate := fields[key]; duplicate {
				return nil, fmt.Errorf("line %d: field %q appears twice", lineNumber+1, key)
			}
			fields[key] = value
		}

		records = append(records, maintenanceRecord{Kind: kind, Fields: fields})
	}

	if !done {
		return nil, fmt.Errorf("report does not end with DONE")
	}

	return records, nil
}

func containsMaintenanceRecord(records []maintenanceRecord, kind string, matching map[string]string) bool {
	_, found := findMaintenanceRecord(records, kind, matching)
	return found
}

func findMaintenanceRecord(records []maintenanceRecord, kind string, matching map[string]string) (maintenanceRecord, bool) {
	for _, record := range records {
		if record.Kind != kind {
			continue
		}
		match := true
		for k, v := range matching {
			if record.Fields[k] != v {
				match = false
				break
			}
		}
		if match {
			return record, true
		}
	}
	return maintenanceRecord{}, false
}

// TestParseMaintenanceATSPIRecords validates the AT-SPI record protocol on
// any host, independent of GTK, Xvfb, or accessibility bus availability.
func TestParseMaintenanceATSPIRecords(t *testing.T) {
	pwTitle, _ := pageview.PowerwashConfirmation()
	frTitle, _ := pageview.FactoryResetConfirmation()

	sampleReport := strings.Join([]string{
		"PAGE\tname=Maintenance\tselected=1",
		fmt.Sprintf("BUTTON\tname=%s\trole=push button\tsensitive=1", cleanupview.ButtonLabel),
		fmt.Sprintf("STATE\tname=%s\tstatus=busy", cleanupview.ButtonLabel),
		fmt.Sprintf("STATE\tname=%s\tstatus=completed\tsensitive=1", cleanupview.ButtonLabel),
		"BUTTON\tname=Remove Everything\trole=push button",
		fmt.Sprintf("DIALOG\ttype=powerwash\ttitle=%s\thas_cancel=1\thas_confirm=1\tbody_valid=1", pwTitle),
		"DIALOG_CANCELLED\ttype=powerwash\tdismissed=1",
		"DIALOG_CONFIRMED\ttype=powerwash\tstatus=completed",
		"BUTTON\tname=Factory Reset\trole=push button",
		fmt.Sprintf("DIALOG\ttype=factory_reset\ttitle=%s\thas_cancel=1\thas_confirm=1\tbody_valid=1", frTitle),
		"DIALOG_CANCELLED\ttype=factory_reset\tdismissed=1",
		"DIALOG_CONFIRMED\ttype=factory_reset\tstatus=completed",
		"DONE",
		"",
	}, "\n")

	records, err := parseMaintenanceATSPIRecords(sampleReport)
	if err != nil {
		t.Fatalf("parseMaintenanceATSPIRecords() error = %v", err)
	}

	if !containsMaintenanceRecord(records, "PAGE", map[string]string{
		"name":     "Maintenance",
		"selected": "1",
	}) {
		t.Errorf("records missing PAGE record for Maintenance")
	}

	if !containsMaintenanceRecord(records, "BUTTON", map[string]string{
		"name":      cleanupview.ButtonLabel,
		"role":      "push button",
		"sensitive": "1",
	}) {
		t.Errorf("records missing BUTTON record for %q", cleanupview.ButtonLabel)
	}

	if !containsMaintenanceRecord(records, "STATE", map[string]string{
		"name":   cleanupview.ButtonLabel,
		"status": "busy",
	}) {
		t.Errorf("records missing busy STATE record for %q", cleanupview.ButtonLabel)
	}

	if !containsMaintenanceRecord(records, "STATE", map[string]string{
		"name":      cleanupview.ButtonLabel,
		"status":    "completed",
		"sensitive": "1",
	}) {
		t.Errorf("records missing completed STATE record for %q", cleanupview.ButtonLabel)
	}

	pwDialog, found := findMaintenanceRecord(records, "DIALOG", map[string]string{"type": "powerwash"})
	if !found {
		t.Fatalf("records missing DIALOG record for powerwash")
	}
	if pwDialog.Fields["title"] != pwTitle {
		t.Errorf("powerwash dialog title = %q, want %q", pwDialog.Fields["title"], pwTitle)
	}
	if pwDialog.Fields["has_cancel"] != "1" || pwDialog.Fields["has_confirm"] != "1" || pwDialog.Fields["body_valid"] != "1" {
		t.Errorf("powerwash dialog fields unexpected: %+v", pwDialog.Fields)
	}

	if !containsMaintenanceRecord(records, "DIALOG_CANCELLED", map[string]string{
		"type":      "powerwash",
		"dismissed": "1",
	}) {
		t.Errorf("records missing DIALOG_CANCELLED record for powerwash")
	}

	if !containsMaintenanceRecord(records, "DIALOG_CONFIRMED", map[string]string{
		"type":   "powerwash",
		"status": "completed",
	}) {
		t.Errorf("records missing DIALOG_CONFIRMED record for powerwash")
	}

	frDialog, found := findMaintenanceRecord(records, "DIALOG", map[string]string{"type": "factory_reset"})
	if !found {
		t.Fatalf("records missing DIALOG record for factory_reset")
	}
	if frDialog.Fields["title"] != frTitle {
		t.Errorf("factory_reset dialog title = %q, want %q", frDialog.Fields["title"], frTitle)
	}
	if frDialog.Fields["has_cancel"] != "1" || frDialog.Fields["has_confirm"] != "1" || frDialog.Fields["body_valid"] != "1" {
		t.Errorf("factory_reset dialog fields unexpected: %+v", frDialog.Fields)
	}

	if !containsMaintenanceRecord(records, "DIALOG_CANCELLED", map[string]string{
		"type":      "factory_reset",
		"dismissed": "1",
	}) {
		t.Errorf("records missing DIALOG_CANCELLED record for factory_reset")
	}

	if !containsMaintenanceRecord(records, "DIALOG_CONFIRMED", map[string]string{
		"type":   "factory_reset",
		"status": "completed",
	}) {
		t.Errorf("records missing DIALOG_CONFIRMED record for factory_reset")
	}
}

// TestParseMaintenanceATSPIRejectsMalformedReports ensures corrupt or truncated
// probe outputs are rejected strictly.
func TestParseMaintenanceATSPIRejectsMalformedReports(t *testing.T) {
	failures := []struct {
		name   string
		report string
		want   string
	}{
		{
			name:   "empty report",
			report: "",
			want:   "empty AT-SPI report",
		},
		{
			name:   "missing DONE",
			report: "PAGE\tname=Maintenance\tselected=1\n",
			want:   "does not end with DONE",
		},
		{
			name:   "record after DONE",
			report: "DONE\nPAGE\tname=Maintenance\tselected=1\n",
			want:   "follows DONE",
		},
		{
			name:   "field missing value",
			report: "PAGE\tname\nDONE\n",
			want:   "not key=value",
		},
		{
			name:   "field missing key",
			report: "PAGE\t=Maintenance\nDONE\n",
			want:   "not key=value",
		},
		{
			name:   "duplicate field",
			report: "PAGE\tname=Maintenance\tname=Other\nDONE\n",
			want:   "appears twice",
		},
		{
			name:   "DONE with trailing fields",
			report: "DONE\textra=1\n",
			want:   "trailing fields",
		},
	}

	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			_, err := parseMaintenanceATSPIRecords(failure.report)
			if err == nil {
				t.Fatalf("parseMaintenanceATSPIRecords(%q) succeeded, want error", failure.report)
			}
			if !strings.Contains(err.Error(), failure.want) {
				t.Errorf("parseMaintenanceATSPIRecords(%q) error = %v, want substring %q", failure.report, err, failure.want)
			}
		})
	}
}

// TestMaintenanceCleanupAndRecoveryThroughATSPI exercises the live AT-SPI
// interaction pass for Maintenance space cleanup and recovery confirmation
// dialogs. When the host lacks the accessibility stack (Xvfb, dbus-run-session,
// dogtail), it skips gracefully without failing the test suite.
func TestMaintenanceCleanupAndRecoveryThroughATSPI(t *testing.T) {
	app := filepath.Join(e2eBuildDir(t), "chairlift")
	requireExecutable(t, app)

	script := filepath.Join(repoRoot(t), "test", "e2e", "run_maintenance_atspi.sh")
	requireExecutable(t, script)

	requireATSPIStack(t)

	outDir := t.TempDir()
	cmd := exec.Command(script, app, outDir)
	cmd.Dir = repoRoot(t)
	output := &lockedBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output

	if err := runWithTimeout(cmd, maintenanceATSPIWaitTimeout); err != nil {
		t.Fatalf("AT-SPI maintenance probe failed: %v\n%s\n%s", err, output.String(), atspiLogs(outDir))
	}

	raw, err := os.ReadFile(filepath.Join(outDir, "atspi-results.txt"))
	if err != nil {
		t.Fatalf("read probe results: %v\n%s", err, atspiLogs(outDir))
	}

	records, err := parseMaintenanceATSPIRecords(string(raw))
	if err != nil {
		t.Fatalf("parse probe results: %v\nresults:\n%s\n%s", err, raw, atspiLogs(outDir))
	}

	if !containsMaintenanceRecord(records, "PAGE", map[string]string{
		"name":     "Maintenance",
		"selected": "1",
	}) {
		t.Errorf("Maintenance page was not reached or selected in AT-SPI tree")
	}

	if !containsMaintenanceRecord(records, "STATE", map[string]string{
		"name":   cleanupview.ButtonLabel,
		"status": "busy",
	}) {
		t.Errorf("clean up action did not enter busy loading state")
	}

	if !containsMaintenanceRecord(records, "STATE", map[string]string{
		"name":      cleanupview.ButtonLabel,
		"status":    "completed",
		"sensitive": "1",
	}) {
		t.Errorf("clean up action did not complete back to sensitive state")
	}

	pwTitle, _ := pageview.PowerwashConfirmation()
	pwDialog, found := findMaintenanceRecord(records, "DIALOG", map[string]string{"type": "powerwash"})
	if !found {
		t.Fatalf("powerwash confirmation dialog was not observed")
	}
	if pwDialog.Fields["title"] != pwTitle {
		t.Errorf("powerwash dialog title = %q, want %q", pwDialog.Fields["title"], pwTitle)
	}
	if pwDialog.Fields["body_valid"] != "1" {
		t.Errorf("powerwash dialog body text did not match expected pageview content")
	}

	if !containsMaintenanceRecord(records, "DIALOG_CANCELLED", map[string]string{"type": "powerwash"}) {
		t.Errorf("powerwash cancel dismissal was not observed")
	}

	if !containsMaintenanceRecord(records, "DIALOG_CONFIRMED", map[string]string{"type": "powerwash", "status": "completed"}) {
		t.Errorf("powerwash confirmation completion was not observed")
	}

	frTitle, _ := pageview.FactoryResetConfirmation()
	frDialog, found := findMaintenanceRecord(records, "DIALOG", map[string]string{"type": "factory_reset"})
	if !found {
		t.Fatalf("factory reset confirmation dialog was not observed")
	}
	if frDialog.Fields["title"] != frTitle {
		t.Errorf("factory reset dialog title = %q, want %q", frDialog.Fields["title"], frTitle)
	}
	if frDialog.Fields["body_valid"] != "1" {
		t.Errorf("factory reset dialog body text did not match expected pageview content")
	}

	if !containsMaintenanceRecord(records, "DIALOG_CANCELLED", map[string]string{"type": "factory_reset"}) {
		t.Errorf("factory reset cancel dismissal was not observed")
	}

	if !containsMaintenanceRecord(records, "DIALOG_CONFIRMED", map[string]string{"type": "factory_reset", "status": "completed"}) {
		t.Errorf("factory reset confirmation completion was not observed")
	}

	chairliftLog, err := os.ReadFile(filepath.Join(outDir, "chairlift.log"))
	if err != nil {
		t.Fatalf("read chairlift.log: %v", err)
	}
	logText := string(chairliftLog)

	if !strings.Contains(logText, "views: reset group built") {
		t.Errorf("chairlift log missing reset group initialization marker")
	}
	if !strings.Contains(logText, "views: powerwash finished") {
		t.Errorf("chairlift log missing powerwash execution marker")
	}
	if !strings.Contains(logText, "factory-reset") {
		t.Errorf("chairlift log missing factory reset execution marker")
	}
}
