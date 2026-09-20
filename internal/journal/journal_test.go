package journal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withSink(t *testing.T, path string) {
	t.Helper()
	t.Setenv(PathEnv, path)
	Reset()
	t.Cleanup(Reset)
}

func readEntries(t *testing.T, path string) []Entry {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening journal %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	var entries []Entry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("decoding journal line %q: %v", scanner.Text(), err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning journal: %v", err)
	}
	return entries
}

// With no sink configured — the state of every ordinary run — Record must
// do nothing: no file created, no error, no panic.
func TestRecordIsANoOpWithoutASink(t *testing.T) {
	t.Setenv(PathEnv, "")
	Reset()
	t.Cleanup(Reset)

	if Enabled() {
		t.Fatal("Enabled() = true with no sink configured")
	}

	Record("channel-switch", map[string]string{"channel": "testing"}, []string{"bootc", "switch"}, SuppressedDryRun)

	if _, err := os.Stat(filepath.Join(t.TempDir(), "journal.jsonl")); err == nil {
		t.Fatal("Record created a file with no sink configured")
	}
}

func TestRecordWritesOneLinePerEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	withSink(t, path)

	Record("channel-switch", map[string]string{"channel": "testing"},
		[]string{"pkexec", "/usr/bin/chairlift-ublue-helper", "channel-switch", "testing"}, SuppressedDryRun)
	Record("restart", nil, []string{"pkexec", "/usr/bin/chairlift-ublue-helper", "restart"}, SuppressedNone)

	entries := readEntries(t, path)
	if len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(entries))
	}

	first := entries[0]
	if first.Action != "channel-switch" {
		t.Errorf("first entry action = %q, want channel-switch", first.Action)
	}
	if first.Args["channel"] != "testing" {
		t.Errorf("first entry args = %v, want channel=testing", first.Args)
	}
	wantArgv := []string{"pkexec", "/usr/bin/chairlift-ublue-helper", "channel-switch", "testing"}
	if len(first.WouldRun) != len(wantArgv) {
		t.Fatalf("first entry WouldRun = %v, want %v", first.WouldRun, wantArgv)
	}
	for i, arg := range wantArgv {
		if first.WouldRun[i] != arg {
			t.Errorf("WouldRun[%d] = %q, want %q", i, first.WouldRun[i], arg)
		}
	}
	if first.Suppressed != SuppressedDryRun {
		t.Errorf("first entry suppressed = %q, want %q", first.Suppressed, SuppressedDryRun)
	}
	if first.Seq != 1 {
		t.Errorf("first entry seq = %d, want 1", first.Seq)
	}
	if entries[1].Seq != 2 {
		t.Errorf("second entry seq = %d, want 2", entries[1].Seq)
	}
	if entries[1].Suppressed != SuppressedNone {
		t.Errorf("second entry suppressed = %q, want %q (a live run)", entries[1].Suppressed, SuppressedNone)
	}
}

// Seq must stay strictly increasing under concurrent callers, since tests
// that care about ordering rely on it rather than on wall-clock time.
func TestRecordSequencesConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	withSink(t, path)

	const writers = 20
	done := make(chan struct{})
	for i := 0; i < writers; i++ {
		go func(n int) {
			Record("concurrent", map[string]string{"n": string(rune('a' + n))}, nil, SuppressedNone)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < writers; i++ {
		<-done
	}

	entries := readEntries(t, path)
	if len(entries) != writers {
		t.Fatalf("journal has %d entries, want %d", len(entries), writers)
	}
	seen := make(map[uint64]bool, writers)
	for _, entry := range entries {
		if seen[entry.Seq] {
			t.Errorf("seq %d appears more than once", entry.Seq)
		}
		seen[entry.Seq] = true
	}
	for n := uint64(1); n <= writers; n++ {
		if !seen[n] {
			t.Errorf("seq %d is missing; sequence has a gap", n)
		}
	}
}

func TestRecordUsesAnRFC3339Timestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	withSink(t, path)

	Record("restart", nil, []string{"systemctl", "reboot"}, SuppressedNone)

	entries := readEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("journal has %d entries, want 1", len(entries))
	}
	if _, err := time.Parse(time.RFC3339, entries[0].Timestamp); err != nil {
		t.Errorf("timestamp %q does not parse as RFC 3339: %v", entries[0].Timestamp, err)
	}
}

// A journal that cannot be written (bad path) must not panic or propagate an
// error: the caller's actual job is the privileged action, not the journal.
func TestRecordToleratesAnUnwritableSink(t *testing.T) {
	withSink(t, filepath.Join(t.TempDir(), "does-not-exist", "journal.jsonl"))

	Record("restart", nil, []string{"systemctl", "reboot"}, SuppressedNone)
	// No assertion beyond "did not panic" — that is the whole contract.
}

func TestResetClearsTheSequenceCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	withSink(t, path)

	Record("a", nil, nil, SuppressedNone)
	Record("b", nil, nil, SuppressedNone)

	path2 := filepath.Join(t.TempDir(), "journal2.jsonl")
	t.Setenv(PathEnv, path2)
	Reset()

	Record("c", nil, nil, SuppressedNone)

	entries := readEntries(t, path2)
	if len(entries) != 1 || entries[0].Seq != 1 {
		t.Fatalf("after Reset, sequence did not restart: %+v", entries)
	}
}

func TestRecordEntrySerializesStatusRootCommandsAndError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	withSink(t, path)

	RecordEntry(Entry{
		Action:       "channel-switch",
		Status:       StatusAttempt,
		Args:         map[string]string{"channel": "testing"},
		WouldRun:     []string{"pkexec", "/usr/bin/chairlift-ublue-helper", "channel-switch", "testing"},
		RootCommands: [][]string{{"bootc", "switch", "--enforce-container-sigpolicy", "ghcr.io/ublue-os/bluefin:testing"}},
		Suppressed:   SuppressedNone,
	})

	RecordEntry(Entry{
		Action:       "channel-switch",
		Status:       StatusDenied,
		Args:         map[string]string{"channel": "testing"},
		WouldRun:     []string{"pkexec", "/usr/bin/chairlift-ublue-helper", "channel-switch", "testing"},
		RootCommands: [][]string{{"bootc", "switch", "--enforce-container-sigpolicy", "ghcr.io/ublue-os/bluefin:testing"}},
		Suppressed:   SuppressedNone,
		Error:        "command failed (exit 126): authorization denied",
	})

	RecordEntry(Entry{
		Action:     "channel-switch",
		Status:     StatusRefused,
		Args:       map[string]string{"channel": "testing"},
		Suppressed: SuppressedRefused,
		Error:      "no testing image is defined for the running tag \"stable\"",
	})

	entries := readEntries(t, path)
	if len(entries) != 3 {
		t.Fatalf("journal has %d entries, want 3", len(entries))
	}

	attempt := entries[0]
	if attempt.Status != StatusAttempt {
		t.Errorf("attempt status = %q, want %q", attempt.Status, StatusAttempt)
	}
	if len(attempt.RootCommands) != 1 || len(attempt.RootCommands[0]) != 4 || attempt.RootCommands[0][0] != "bootc" {
		t.Errorf("attempt root commands = %v, want one bootc switch argv", attempt.RootCommands)
	}

	denied := entries[1]
	if denied.Status != StatusDenied {
		t.Errorf("denied status = %q, want %q", denied.Status, StatusDenied)
	}
	if denied.Error != "command failed (exit 126): authorization denied" {
		t.Errorf("denied error = %q", denied.Error)
	}

	refused := entries[2]
	if refused.Status != StatusRefused {
		t.Errorf("refused status = %q, want %q", refused.Status, StatusRefused)
	}
	if refused.Suppressed != SuppressedRefused {
		t.Errorf("refused suppressed = %q, want %q", refused.Suppressed, SuppressedRefused)
	}
}
