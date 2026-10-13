package ubluehelper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// fakeRunner records every command a Runner is asked to execute and answers
// each from a scripted queue of results. A result's stderr is written to the
// capture buffer when the caller supplies one, which is how SwitchImage sees
// the composefs refusal text.
type fakeRunner struct {
	calls   []string
	results []fakeResult
}

type fakeResult struct {
	stderr string
	err    error
}

func (f *fakeRunner) run(_ context.Context, stderr io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if len(f.results) == 0 {
		return nil
	}
	result := f.results[0]
	f.results = f.results[1:]
	if stderr != nil && result.stderr != "" {
		_, _ = fmt.Fprint(stderr, result.stderr)
	}
	return result.err
}

const composefsRefusal = "ERROR: Target image has the same fs-verity digest as the existing Some(Rollback) deployment\n"

func TestSwitchImageOutcomes(t *testing.T) {
	const target = "ghcr.io/projectbluefin/bluefin:stable"
	switchArgs := []string{"switch", "--enforce-container-sigpolicy", target}
	switchCall := "bootc switch --enforce-container-sigpolicy " + target
	const rollbackCall = "bootc rollback"
	switchErr := errors.New("exit status 1")
	rollbackErr := errors.New("exit status 2")

	cases := []struct {
		name        string
		results     []fakeResult
		rollbackRef string
		rollbackErr error
		wantCalls   []string
		wantLine    string
		wantErr     string
	}{
		{
			name:      "switch succeeds",
			results:   []fakeResult{{}},
			wantCalls: []string{switchCall},
			wantLine:  "switched to " + target + " — restart to apply",
		},
		{
			name:        "refusal for the rollback deployment recovers via bootc rollback",
			results:     []fakeResult{{stderr: composefsRefusal, err: switchErr}, {}},
			rollbackRef: target,
			wantCalls:   []string{switchCall, rollbackCall},
			wantLine:    "switched to " + target + " (the previous deployment) — restart to apply",
		},
		{
			name:        "refusal recovered but bootc rollback fails",
			results:     []fakeResult{{stderr: composefsRefusal, err: switchErr}, {err: rollbackErr}},
			rollbackRef: target,
			wantCalls:   []string{switchCall, rollbackCall},
			wantErr:     "bootc switch failed: exit status 1; rollback to " + target + " failed: exit status 2",
		},
		{
			name:      "plain failure is reported without a rollback attempt",
			results:   []fakeResult{{stderr: "ERROR: pulling image: manifest unknown\n", err: switchErr}},
			wantCalls: []string{switchCall},
			wantErr:   "bootc switch failed: exit status 1",
		},
		{
			name:        "refusal whose rollback deployment is a different image is a plain failure",
			results:     []fakeResult{{stderr: composefsRefusal, err: switchErr}},
			rollbackRef: "ghcr.io/projectbluefin/bluefin:testing",
			wantCalls:   []string{switchCall},
			wantErr:     "bootc switch failed: exit status 1",
		},
		{
			name:        "refusal with an unreadable bootc status is a plain failure",
			results:     []fakeResult{{stderr: composefsRefusal, err: switchErr}},
			rollbackErr: errors.New("bootc status: exit status 1"),
			wantCalls:   []string{switchCall},
			wantErr:     "bootc switch failed: exit status 1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{results: tc.results}
			rollbackRef := func(context.Context) (string, error) {
				return tc.rollbackRef, tc.rollbackErr
			}

			line, err := SwitchImage(context.Background(), runner.run, rollbackRef, switchArgs, "bootc switch failed")

			if strings.Join(runner.calls, "\n") != strings.Join(tc.wantCalls, "\n") {
				t.Errorf("commands run:\n%s\nwant:\n%s", strings.Join(runner.calls, "\n"), strings.Join(tc.wantCalls, "\n"))
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if line != tc.wantLine {
					t.Errorf("line = %q, want %q", line, tc.wantLine)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
			if line != "" {
				t.Errorf("line = %q on failure, want empty", line)
			}
		})
	}
}

func TestSwitchImageOnlyConsultsStatusAfterTheRefusal(t *testing.T) {
	lookups := 0
	rollbackRef := func(context.Context) (string, error) {
		lookups++
		return "", nil
	}
	runner := &fakeRunner{results: []fakeResult{{stderr: "ERROR: something else\n", err: errors.New("exit status 1")}}}
	if _, err := SwitchImage(context.Background(), runner.run, rollbackRef, []string{"switch", "x"}, "driver switch failed"); err == nil {
		t.Fatal("expected an error")
	}
	if lookups != 0 {
		t.Errorf("bootc status was read %d times on a non-refusal failure, want 0", lookups)
	}
}

func TestSwitchImageRefusesAnEmptyArgv(t *testing.T) {
	runner := &fakeRunner{}
	_, err := SwitchImage(context.Background(), runner.run, func(context.Context) (string, error) { return "", nil }, nil, "bootc switch failed")
	if err == nil || !strings.HasPrefix(err.Error(), "bootc switch failed: ") {
		t.Fatalf("err = %v, want a prefixed refusal", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %v with no argv", runner.calls)
	}
}

// groupScript answers ApplyDevGroups' ensure and run seams per group name.
type groupScript struct {
	ensureErr map[string]error
	runErr    map[string]error
	ensured   []string
	runs      []string
}

func (g *groupScript) ensure(group string, dryRun bool) error {
	g.ensured = append(g.ensured, fmt.Sprintf("%s dry=%t", group, dryRun))
	return g.ensureErr[group]
}

func (g *groupScript) run(_ context.Context, stderr io.Writer, name string, args ...string) error {
	if stderr != nil {
		return errors.New("dev-group commands must not request stderr capture")
	}
	g.runs = append(g.runs, strings.Join(append([]string{name}, args...), " "))
	// usermod -aG GROUP USER / gpasswd -d USER GROUP: the group is the
	// argument that is not the username.
	for _, arg := range args[1:] {
		if arg != "dev" {
			if err, failed := g.runErr[arg]; failed {
				return err
			}
		}
	}
	return nil
}

func TestApplyDevGroupsEnableAppliesEveryGroup(t *testing.T) {
	script := &groupScript{}
	var stdout, stderr strings.Builder
	applied, err := ApplyDevGroups(context.Background(), script.run, script.ensure, CommandDXEnable, "dev", false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != len(DevGroups()) {
		t.Errorf("applied = %d, want %d", applied, len(DevGroups()))
	}
	wantRuns := []string{
		"usermod -aG docker dev",
		"usermod -aG incus-admin dev",
		"usermod -aG libvirt dev",
		"usermod -aG dialout dev",
	}
	if strings.Join(script.runs, "\n") != strings.Join(wantRuns, "\n") {
		t.Errorf("runs:\n%s\nwant:\n%s", strings.Join(script.runs, "\n"), strings.Join(wantRuns, "\n"))
	}
	if len(script.ensured) != len(DevGroups()) || script.ensured[0] != "docker dry=false" {
		t.Errorf("ensured = %v, want every group with dry=false", script.ensured)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("stdout=%q stderr=%q, want both empty on a clean live run", stdout.String(), stderr.String())
	}
}

func TestApplyDevGroupsDisableSkipsEnsureAndUsesGpasswd(t *testing.T) {
	script := &groupScript{}
	var stdout, stderr strings.Builder
	applied, err := ApplyDevGroups(context.Background(), script.run, script.ensure, CommandDXDisable, "dev", false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != len(DevGroups()) {
		t.Errorf("applied = %d, want %d", applied, len(DevGroups()))
	}
	if len(script.ensured) != 0 {
		t.Errorf("disable ensured groups %v; only enable copies image-only groups", script.ensured)
	}
	if script.runs[0] != "gpasswd -d dev docker" {
		t.Errorf("first run = %q, want gpasswd -d dev docker", script.runs[0])
	}
}

func TestApplyDevGroupsPartialFailureSkipsAndCounts(t *testing.T) {
	ensureErr := errors.New("group not in /usr/lib/group")
	runErr := errors.New("exit status 6")
	script := &groupScript{
		ensureErr: map[string]error{"incus-admin": ensureErr},
		runErr:    map[string]error{"libvirt": runErr},
	}
	var stdout, stderr strings.Builder
	applied, err := ApplyDevGroups(context.Background(), script.run, script.ensure, CommandDXEnable, "dev", false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != 2 {
		t.Errorf("applied = %d, want 2 (docker and dialout)", applied)
	}
	wantRuns := []string{"usermod -aG docker dev", "usermod -aG libvirt dev", "usermod -aG dialout dev"}
	if strings.Join(script.runs, "\n") != strings.Join(wantRuns, "\n") {
		t.Errorf("runs:\n%s\nwant (incus-admin never reaches usermod):\n%s", strings.Join(script.runs, "\n"), strings.Join(wantRuns, "\n"))
	}
	wantStderr := SkippedGroupLine("incus-admin", ensureErr) + "\n" + SkippedGroupLine("libvirt", runErr) + "\n"
	if stderr.String() != wantStderr {
		t.Errorf("stderr = %q, want %q", stderr.String(), wantStderr)
	}
	if got := SkippedGroups(stderr.String()); strings.Join(got, ",") != "incus-admin,libvirt" {
		t.Errorf("SkippedGroups reads %v from the skipped lines, want incus-admin,libvirt", got)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestApplyDevGroupsFailsWhenNoGroupApplied(t *testing.T) {
	runErr := errors.New("exit status 6")
	script := &groupScript{runErr: map[string]error{}}
	for _, group := range DevGroups() {
		script.runErr[group] = runErr
	}
	var stdout, stderr strings.Builder
	applied, err := ApplyDevGroups(context.Background(), script.run, script.ensure, CommandDXDisable, "dev", false, &stdout, &stderr)
	if !errors.Is(err, ErrNoDevGroupsChanged) {
		t.Fatalf("err = %v, want ErrNoDevGroupsChanged", err)
	}
	if err.Error() != "no developer groups could be changed" {
		t.Errorf("error text = %q", err.Error())
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0", applied)
	}
	if got := SkippedGroups(stderr.String()); len(got) != len(DevGroups()) {
		t.Errorf("stderr reports %v skipped, want all %d groups", got, len(DevGroups()))
	}
}

func TestApplyDevGroupsDryRunPrintsInsteadOfRunning(t *testing.T) {
	script := &groupScript{}
	var stdout, stderr strings.Builder
	applied, err := ApplyDevGroups(context.Background(), script.run, script.ensure, CommandDXEnable, "dev", true, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if applied != len(DevGroups()) {
		t.Errorf("applied = %d, want %d", applied, len(DevGroups()))
	}
	if len(script.runs) != 0 {
		t.Errorf("dry run executed %v", script.runs)
	}
	if script.ensured[0] != "docker dry=true" {
		t.Errorf("ensured = %v, want the dry-run flag passed through", script.ensured)
	}
	// The e2e suite matches these exact prefixes on the staged binary's output.
	want := "[DRY-RUN] would execute: usermod [-aG docker dev]\n" +
		"[DRY-RUN] would execute: usermod [-aG incus-admin dev]\n" +
		"[DRY-RUN] would execute: usermod [-aG libvirt dev]\n" +
		"[DRY-RUN] would execute: usermod [-aG dialout dev]\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestApplyDevGroupsRefusesNonDXCommands(t *testing.T) {
	script := &groupScript{}
	var stdout, stderr strings.Builder
	for _, command := range []string{CommandRollback, CommandKVMEnable, ""} {
		_, err := ApplyDevGroups(context.Background(), script.run, script.ensure, command, "dev", false, &stdout, &stderr)
		if err == nil || !strings.HasPrefix(err.Error(), "unsupported group operation for command ") {
			t.Errorf("%q: err = %v, want an unsupported-operation refusal", command, err)
		}
	}
	if len(script.runs) != 0 || len(script.ensured) != 0 {
		t.Errorf("refused commands still ran %v / ensured %v", script.runs, script.ensured)
	}
}

func TestDevGroupsAppliedLine(t *testing.T) {
	got := DevGroupsAppliedLine(CommandDXEnable, 3, "dev")
	want := "dx-enable applied to 3 group(s) for dev — log out and back in to take effect"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
