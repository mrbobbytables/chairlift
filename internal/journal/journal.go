// Package journal records every privileged action ChairLift takes or would
// take, as one JSON object per line, so that the intent behind a click can be
// asserted without granting privilege.
//
// It is a port of finupdate's action_journal, and exists for the reason that
// package documents: a screenshot can prove a row *looks* right, but only a
// journal can prove that pressing Switch would have run
// `bootc switch ghcr.io/projectbluefin/dakota:testing` and not the currently
// booted reference. ChairLift's dry-run mode already short-circuits every
// mutation before pkexec, and its log lines are human-readable — which makes
// the interesting half of the walkthrough unassertable. The journal is the
// machine-readable half.
//
// This is not one of the chairlift_e2e stubs. It changes no behaviour: with
// $CHAIRLIFT_ACTION_JOURNAL unset — which is every ordinary run — Record is a
// couple of atomic loads and returns. It ships in the released binary
// deliberately, because a journal captured from a real system is also the
// audit trail for what ChairLift did to that machine, which matters most for
// the operations that cannot be undone.
package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// PathEnv names the JSONL sink. Unset means journalling is disabled.
const PathEnv = "CHAIRLIFT_ACTION_JOURNAL"

// Suppression records why an action was journalled rather than executed.
type Suppression string

const (
	// SuppressedNone means the action really ran. Recorded so a journal
	// captured on a real system still shows the complete action sequence,
	// not only the things that were skipped.
	SuppressedNone Suppression = "no"
	// SuppressedDryRun means --dry-run short-circuited the action before it
	// reached pkexec.
	SuppressedDryRun Suppression = "dry-run"
	// SuppressedRefused means ChairLift declined to build a command at all —
	// an unswitchable channel, an unpublished driver image. This is the case
	// a test most wants to see, because "nothing happened" and "the wrong
	// thing was almost attempted" look identical from outside.
	SuppressedRefused Suppression = "refused"
)

// Status records the outcome or lifecycle stage of a privileged action.
type Status string

const (
	// StatusAttempt records that a live privileged action is being attempted
	// (dispatched to PolicyKit / the privileged helper).
	StatusAttempt Status = "attempt"
	// StatusSuccess records that a live privileged action completed successfully.
	StatusSuccess Status = "success"
	// StatusFailure records that a privileged action failed with an error.
	StatusFailure Status = "failure"
	// StatusDenied records that PolicyKit denied or dismissed authentication.
	StatusDenied Status = "denied"
	// StatusTimeout records that the action timed out before completing.
	StatusTimeout Status = "timeout"
	// StatusRefused records that ChairLift declined to build or execute the command.
	StatusRefused Status = "refused"
	// StatusDryRun records a preview under --dry-run.
	StatusDryRun Status = "dry-run"
)

// Entry is one journalled action.
type Entry struct {
	// Seq is a process-wide monotonic sequence number, assigned under the
	// same lock that serialises the append, so entries are totally ordered
	// even when several goroutines journal concurrently. Timestamp is for
	// humans; ordering questions are answered by Seq.
	Seq uint64 `json:"seq"`
	// Action is the operation name, e.g. "channel-switch".
	Action string `json:"action"`
	// Status records whether the action was an attempt, dry-run preview,
	// refusal, or its final execution outcome (success, denied, timeout, failure).
	Status Status `json:"status,omitempty"`
	// Args are the operation's inputs as ChairLift understood them.
	Args map[string]string `json:"args,omitempty"`
	// WouldRun is the argv a real run executes, verbatim. Keeping the built
	// command rather than re-deriving it in a test is the whole point: the
	// assertion then checks the command ChairLift actually assembled.
	WouldRun []string `json:"would_run,omitempty"`
	// RootCommand is the expected concrete privileged command resolved for
	// the helper action (e.g. `bootc switch --enforce-container-sigpolicy <target>`).
	RootCommand []string `json:"root_command,omitempty"`
	// Suppressed records whether the action ran.
	Suppressed Suppression `json:"suppressed"`
	// Error records the failure or refusal message, if any.
	Error string `json:"error,omitempty"`
	// Timestamp is RFC 3339 UTC, for human reading only.
	Timestamp string `json:"ts"`
}

var (
	// enabled caches whether a sink is configured, so the disabled path
	// costs an atomic load rather than a getenv per action.
	enabled atomic.Bool
	// configured guards one-time initialisation.
	configured sync.Once

	mu   sync.Mutex
	seq  uint64
	sink string

	// now is an injection seam so entries are deterministic under test.
	now = func() time.Time { return time.Now().UTC() }
)

func initialise() {
	sink = os.Getenv(PathEnv)
	enabled.Store(sink != "")
}

// Enabled reports whether a journal sink is configured.
func Enabled() bool {
	configured.Do(initialise)
	return enabled.Load()
}

// RecordEntry appends one entry directly, deriving default values for Seq,
// Timestamp, Suppressed, or Status if they are omitted. It never returns an
// error and never panics: a write failure is silently dropped.
func RecordEntry(entry Entry) {
	if !Enabled() {
		return
	}

	mu.Lock()
	defer mu.Unlock()

	seq++
	entry.Seq = seq
	if entry.Timestamp == "" {
		entry.Timestamp = now().Format(time.RFC3339)
	}
	if entry.Suppressed == "" {
		switch entry.Status {
		case StatusDryRun:
			entry.Suppressed = SuppressedDryRun
		case StatusRefused:
			entry.Suppressed = SuppressedRefused
		default:
			entry.Suppressed = SuppressedNone
		}
	}
	if entry.Status == "" {
		switch entry.Suppressed {
		case SuppressedDryRun:
			entry.Status = StatusDryRun
		case SuppressedRefused:
			entry.Status = StatusRefused
		default:
			entry.Status = StatusSuccess
		}
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return
	}

	file, err := os.OpenFile(sink, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = fmt.Fprintf(file, "%s\n", line)
}

// Record appends one entry using legacy arguments. It sets Status
// based on suppressed.
func Record(action string, args map[string]string, wouldRun []string, suppressed Suppression) {
	RecordEntry(Entry{
		Action:     action,
		Args:       args,
		WouldRun:   wouldRun,
		Suppressed: suppressed,
	})
}

// Reset reconfigures the journal from the environment and clears the
// sequence counter. It exists for tests, which need to point the sink at a
// temporary file after the package has already initialised.
func Reset() {
	mu.Lock()
	defer mu.Unlock()

	configured = sync.Once{}
	seq = 0
	initialise()
}
