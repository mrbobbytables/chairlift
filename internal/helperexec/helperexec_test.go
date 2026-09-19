package helperexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/projectbluefin/chairlift/internal/dryrun"
	"github.com/projectbluefin/chairlift/internal/imageinfo"
	"github.com/projectbluefin/chairlift/internal/journal"
	"github.com/projectbluefin/chairlift/internal/ubluehelper"
)

// writeFakePkexec writes an executable shell script standing in for pkexec:
// it records its own argv (one element per line) to capturedArgsFile and
// exits 0. It never execs the real pkexec or requires root.
func writeFakePkexec(t *testing.T, capturedArgsFile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-pkexec")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + capturedArgsFile + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake pkexec: %v", err)
	}
	return path
}

func TestRunInvokesPkexecWithFixedHelperPathAndArgs(t *testing.T) {
	dryrun.Set(false)

	capturedArgsFile := filepath.Join(t.TempDir(), "captured-args")
	fakePkexec := writeFakePkexec(t, capturedArgsFile)

	helperPath := "/usr/bin/chairlift-example-helper"
	if _, _, err := Run(context.Background(), fakePkexec, helperPath, "do-thing", "arg1"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(capturedArgsFile)
	if err != nil {
		t.Fatalf("reading captured pkexec argv: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := []string{helperPath, "do-thing", "arg1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pkexec argv = %v, want %v", got, want)
	}
}

func TestRunDryRunNeverInvokesPkexec(t *testing.T) {
	dryrun.Set(true)
	defer dryrun.Set(false)

	// A path that does not exist: if Run failed to short-circuit and tried
	// to actually run it, cmd.Run() would return an error and this test
	// would fail loudly instead of silently passing.
	nonexistentPkexec := filepath.Join(t.TempDir(), "pkexec-should-never-run")

	stdout, stderr, err := Run(context.Background(), nonexistentPkexec, "/usr/bin/chairlift-example-helper", "do-thing")
	if err != nil {
		t.Fatalf("Run dry-run returned error, want short-circuit with nil error: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("Run dry-run returned stdout=%q stderr=%q, want both empty", stdout, stderr)
	}
}

func TestRunClassifiesMissingPkexecAsNotFound(t *testing.T) {
	dryrun.Set(false)

	// A bare name that $PATH lookup cannot resolve: exec reports
	// exec.ErrNotFound, the shape Run classifies as *NotFoundError.
	_, _, err := Run(context.Background(), "chairlift-pkexec-that-does-not-exist", "/usr/bin/chairlift-example-helper", "do-thing")
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Run error = %T (%v), want *NotFoundError", err, err)
	}
	if !strings.Contains(err.Error(), "chairlift-example-helper") {
		t.Fatalf("NotFoundError message = %q, want it to name the helper basename", err.Error())
	}
}

func TestRunClassifiesNonExecutablePkexecAsError(t *testing.T) {
	dryrun.Set(false)

	// A non-executable file: os/exec reports EACCES, which is not
	// exec.ErrNotFound. The classification must fall through the ladder to
	// *Error carrying the OS message — a present-but-unexecutable pkexec is
	// not "pkexec not found".
	notExecutable := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("writing non-executable file: %v", err)
	}

	_, _, err := Run(context.Background(), notExecutable, "/usr/bin/chairlift-example-helper", "do-thing")
	var notFound *NotFoundError
	if errors.As(err, &notFound) {
		t.Fatalf("Run error = %T (%v), want EACCES not to classify as *NotFoundError", err, err)
	}
	var helperErr *Error
	if !errors.As(err, &helperErr) {
		t.Fatalf("Run error = %T (%v), want *Error", err, err)
	}
}

// TestClassifyFailureSeesThroughWrappedErrors pins the guarantee that no
// input to Run can provide: os/exec returns *exec.Error and *exec.ExitError
// unwrapped, so the errors.As ladder and the pre-extraction comma-ok ladder
// internal/updex carried agree on every error Run can observe. The ladders
// diverge only on a wrapped error — and the comma-ok form silently degrades
// a wrapped exec.ErrNotFound to a generic *Error. That drift is the one this
// package exists to make impossible, so it is pinned here at the seam where
// it can actually be exercised.
func TestClassifyFailureSeesThroughWrappedErrors(t *testing.T) {
	wrappedNotFound := fmt.Errorf("spawning pkexec: %w", &exec.Error{Name: "pkexec", Err: exec.ErrNotFound})
	err := classifyFailure(wrappedNotFound, "/usr/bin/chairlift-example-helper", "")
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("classifyFailure(wrapped exec.ErrNotFound) = %T (%v), want *NotFoundError", err, err)
	}
	if !strings.Contains(err.Error(), "chairlift-example-helper") {
		t.Fatalf("NotFoundError message = %q, want it to name the helper basename", err.Error())
	}
}

// TestRunJournalsDryRunFlagAsSuppressed pins the safety-critical half of
// dry-run in the package that owns it. The short-circuit stops pkexec
// spawning (TestRunDryRunNeverInvokesPkexec); the appended --dry-run is what
// tells the helper to preview rather than act on every invocation that does
// reach it, and the journal is the machine-readable record of both.
func TestRunJournalsDryRunFlagAsSuppressed(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")
	t.Setenv(journal.PathEnv, journalPath)
	journal.Reset()
	t.Cleanup(journal.Reset)

	dryrun.Set(true)
	t.Cleanup(func() { dryrun.Set(false) })

	pkexecPath := "pkexec-should-never-run"
	helperPath := "/usr/bin/chairlift-example-helper"
	if _, _, err := Run(context.Background(), pkexecPath, helperPath, "do-thing", "arg1"); err != nil {
		t.Fatalf("Run dry-run error = %v, want nil", err)
	}

	entries := readJournal(t, journalPath)
	if len(entries) != 1 {
		t.Fatalf("journal has %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Action != "do-thing" {
		t.Errorf("journalled action = %q, want %q", entry.Action, "do-thing")
	}
	if entry.Suppressed != journal.SuppressedDryRun {
		t.Errorf("journalled suppressed = %q, want %q", entry.Suppressed, journal.SuppressedDryRun)
	}
	wantArgv := []string{pkexecPath, helperPath, "do-thing", "arg1", "--dry-run"}
	if !reflect.DeepEqual(entry.WouldRun, wantArgv) {
		t.Errorf("journalled WouldRun = %v, want %v (--dry-run appended last)", entry.WouldRun, wantArgv)
	}
	// Args must record only the caller's real inputs, not the internal
	// --dry-run flag appended for WouldRun/logging.
	wantArgs := map[string]string{"args": "arg1"}
	if !reflect.DeepEqual(entry.Args, wantArgs) {
		t.Errorf("journalled Args = %v, want %v (must not include --dry-run)", entry.Args, wantArgs)
	}
}

func TestRunJournalsAttemptAndSuccessOutcome(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")
	t.Setenv(journal.PathEnv, journalPath)
	journal.Reset()
	t.Cleanup(journal.Reset)

	capturedArgsFile := filepath.Join(t.TempDir(), "captured-args")
	fakePkexec := writeFakePkexec(t, capturedArgsFile)

	info := imageinfo.Info{Name: "dakota", Tag: "latest", Ref: "docker://ghcr.io/projectbluefin/dakota"}
	restore := ubluehelper.SetDetectInfo(func() (imageinfo.Info, error) { return info, nil })
	t.Cleanup(restore)

	helperPath := "/usr/bin/chairlift-ublue-helper"
	if _, _, err := Run(context.Background(), fakePkexec, helperPath, "channel-switch", "testing"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries := readJournal(t, journalPath)
	if len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(entries))
	}

	wantRootCmd := []string{"bootc", "switch", "--enforce-container-sigpolicy", "ghcr.io/projectbluefin/dakota:testing"}
	wantWouldRun := []string{fakePkexec, helperPath, "channel-switch", "testing"}

	attempt := entries[0]
	if attempt.Status != journal.StatusAttempt {
		t.Errorf("attempt status = %q, want %q", attempt.Status, journal.StatusAttempt)
	}
	if attempt.Suppressed != journal.SuppressedNone {
		t.Errorf("attempt suppressed = %q, want %q", attempt.Suppressed, journal.SuppressedNone)
	}
	if !reflect.DeepEqual(attempt.WouldRun, wantWouldRun) {
		t.Errorf("attempt WouldRun = %v, want %v", attempt.WouldRun, wantWouldRun)
	}
	if !reflect.DeepEqual(attempt.RootCommand, wantRootCmd) {
		t.Errorf("attempt RootCommand = %v, want %v", attempt.RootCommand, wantRootCmd)
	}

	success := entries[1]
	if success.Status != journal.StatusSuccess {
		t.Errorf("success status = %q, want %q", success.Status, journal.StatusSuccess)
	}
	if success.Suppressed != journal.SuppressedNone {
		t.Errorf("success suppressed = %q, want %q", success.Suppressed, journal.SuppressedNone)
	}
	if !reflect.DeepEqual(success.RootCommand, wantRootCmd) {
		t.Errorf("success RootCommand = %v, want %v", success.RootCommand, wantRootCmd)
	}
}

func TestRunJournalsPolicyKitDenied(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")
	t.Setenv(journal.PathEnv, journalPath)
	journal.Reset()
	t.Cleanup(journal.Reset)

	deniedScript := filepath.Join(t.TempDir(), "denied-pkexec")
	if err := os.WriteFile(deniedScript, []byte("#!/bin/sh\necho 'dismissed' >&2\nexit 126\n"), 0o755); err != nil {
		t.Fatalf("writing denied pkexec: %v", err)
	}

	info := imageinfo.Info{Name: "dakota", Tag: "latest", Ref: "docker://ghcr.io/projectbluefin/dakota"}
	restore := ubluehelper.SetDetectInfo(func() (imageinfo.Info, error) { return info, nil })
	t.Cleanup(restore)

	helperPath := "/usr/bin/chairlift-ublue-helper"
	_, _, err := Run(context.Background(), deniedScript, helperPath, "channel-switch", "testing")
	if err == nil {
		t.Fatal("Run error = nil, want error for exit 126")
	}

	entries := readJournal(t, journalPath)
	if len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(entries))
	}

	if entries[0].Status != journal.StatusAttempt {
		t.Errorf("entry 0 status = %q, want %q", entries[0].Status, journal.StatusAttempt)
	}

	denied := entries[1]
	if denied.Status != journal.StatusDenied {
		t.Errorf("entry 1 status = %q, want %q", denied.Status, journal.StatusDenied)
	}
	if denied.Suppressed != journal.SuppressedNone {
		t.Errorf("entry 1 suppressed = %q, want %q", denied.Suppressed, journal.SuppressedNone)
	}
	if !strings.Contains(denied.Error, "126") {
		t.Errorf("entry 1 error = %q, want exit 126", denied.Error)
	}
	wantRootCmd := []string{"bootc", "switch", "--enforce-container-sigpolicy", "ghcr.io/projectbluefin/dakota:testing"}
	if !reflect.DeepEqual(denied.RootCommand, wantRootCmd) {
		t.Errorf("entry 1 RootCommand = %v, want %v", denied.RootCommand, wantRootCmd)
	}
}

func TestRunJournalsTimeout(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")
	t.Setenv(journal.PathEnv, journalPath)
	journal.Reset()
	t.Cleanup(journal.Reset)

	hangingScript := filepath.Join(t.TempDir(), "hanging-pkexec")
	if err := os.WriteFile(hangingScript, []byte("#!/bin/sh\nsleep 1\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing hanging pkexec: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	helperPath := "/usr/bin/chairlift-ublue-helper"
	_, _, err := Run(ctx, hangingScript, helperPath, "restart")
	if err == nil {
		t.Fatal("Run error = nil, want timeout")
	}

	entries := readJournal(t, journalPath)
	if len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(entries))
	}

	if entries[0].Status != journal.StatusAttempt {
		t.Errorf("entry 0 status = %q, want %q", entries[0].Status, journal.StatusAttempt)
	}
	timeoutEntry := entries[1]
	if timeoutEntry.Status != journal.StatusTimeout {
		t.Errorf("entry 1 status = %q, want %q", timeoutEntry.Status, journal.StatusTimeout)
	}
	if timeoutEntry.Error != "command timed out" {
		t.Errorf("entry 1 error = %q, want command timed out", timeoutEntry.Error)
	}
	wantRootCmd := []string{"systemctl", "reboot"}
	if !reflect.DeepEqual(timeoutEntry.RootCommand, wantRootCmd) {
		t.Errorf("entry 1 RootCommand = %v, want %v", timeoutEntry.RootCommand, wantRootCmd)
	}
}

func TestRunJournalsHelperRefusalOutcome(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")
	t.Setenv(journal.PathEnv, journalPath)
	journal.Reset()
	t.Cleanup(journal.Reset)

	refusingScript := filepath.Join(t.TempDir(), "refusing-pkexec")
	if err := os.WriteFile(refusingScript, []byte("#!/bin/sh\necho 'no testing image is defined for the running tag \"stable\"' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing refusing pkexec: %v", err)
	}

	helperPath := "/usr/bin/chairlift-ublue-helper"
	_, _, err := Run(context.Background(), refusingScript, helperPath, "channel-switch", "testing")
	if err == nil {
		t.Fatal("Run error = nil, want refusal error from helper")
	}

	entries := readJournal(t, journalPath)
	if len(entries) != 2 {
		t.Fatalf("journal has %d entries, want 2", len(entries))
	}
	if entries[0].Status != journal.StatusAttempt {
		t.Errorf("entry 0 status = %q, want %q", entries[0].Status, journal.StatusAttempt)
	}
	refused := entries[1]
	if refused.Status != journal.StatusRefused {
		t.Errorf("refused status = %q, want %q", refused.Status, journal.StatusRefused)
	}
	if refused.Suppressed != journal.SuppressedRefused {
		t.Errorf("refused suppressed = %q, want %q", refused.Suppressed, journal.SuppressedRefused)
	}
	if !strings.Contains(refused.Error, "no testing image is defined") {
		t.Errorf("refused error = %q, want explanation", refused.Error)
	}
}

func readJournal(t *testing.T, path string) []journal.Entry {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading journal %s: %v", path, err)
	}

	var entries []journal.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry journal.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decoding journal line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}
