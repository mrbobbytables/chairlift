// Package helperexec runs ChairLift's fixed-path privileged helper binaries
// via pkexec. It owns the invocation contract shared by internal/ublue and
// internal/updex — previously copied verbatim into both packages, where it
// had already drifted (one classified wrapped exec errors with errors.As,
// the other with comma-ok type assertions that miss wrapped errors).
//
// The contract: the helper path is a fixed absolute path chosen by the
// caller (it must match the polkit policy's exec.path annotation and is
// never overridable here); every invocation — dry-run or live — is recorded
// in internal/journal; dry-run appends --dry-run and never spawns pkexec;
// failure is classified as *NotFoundError (pkexec or helper absent) or
// *Error (timeout, non-zero exit, or any other start/wait failure, with the
// underlying message preserved).
//
// internal/ublue and internal/updex alias both error types, so the taxonomy
// is deliberately shared across helpers: a failure's type never identifies
// which helper failed. A caller that needs that attribution must carry the
// invocation context itself rather than discriminate on the error type.
//
// pkexecPath is a parameter, always "pkexec" in production, so tests can
// substitute a fake pkexec stand-in without invoking the real pkexec/polkit
// stack or requiring root.
//
// Helper-specific knowledge lives outside this package: a helper's owning
// package registers a RootCommandResolver for its basename (see
// RegisterRootCommandResolver), so helperexec stays the helper-agnostic
// invocation contract its taxonomy claims to be.
package helperexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path"
	"strings"
	"sync"

	"github.com/projectbluefin/chairlift/internal/dryrun"
	"github.com/projectbluefin/chairlift/internal/journal"
)

// Error represents a privileged helper invocation failure.
type Error struct {
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

// NotFoundError is returned when pkexec or the privileged helper is absent.
type NotFoundError struct {
	Message string
}

func (e *NotFoundError) Error() string {
	return e.Message
}

// RefusalError is returned by a RootCommandResolver when ChairLift declines
// to build a privileged command at all — an unswitchable channel, an
// unpublished driver image. Run treats it as a refusal: nothing is
// dispatched to pkexec, and the journal records status "refused" with
// suppression "refused", the one state that truthfully means no root process
// ever started.
type RefusalError struct {
	Message string
}

func (e *RefusalError) Error() string {
	return e.Message
}

// RootCommandResolver predicts the concrete privileged command a helper will
// run for args, for the journal's root_command field. It returns a
// *RefusalError when ChairLift declines to build a command at all, and
// (nil, nil) when the command cannot be predicted from the unprivileged
// process — an unpredictable command is journalled without root_command
// rather than blocking the invocation.
type RootCommandResolver func(args []string) ([]string, error)

var (
	resolversMu sync.RWMutex
	resolvers   = map[string]RootCommandResolver{}
)

// RegisterRootCommandResolver registers resolver for the helper binary whose
// basename is helperBase. A helper's owning package calls it from init, so
// helperexec never has to know which helpers exist or what they run. Passing
// a nil resolver removes the registration.
func RegisterRootCommandResolver(helperBase string, resolver RootCommandResolver) {
	resolversMu.Lock()
	defer resolversMu.Unlock()

	if resolver == nil {
		delete(resolvers, helperBase)
		return
	}
	resolvers[helperBase] = resolver
}

// journalArgs turns a privileged helper's argv (minus the leading command
// word, which becomes journal.Entry.Action) into the journal's args map.
func journalArgs(args []string) map[string]string {
	if len(args) <= 1 {
		return nil
	}
	return map[string]string{"args": strings.Join(args[1:], " ")}
}

func resolveRootCommand(helperPath string, args []string) ([]string, error) {
	resolversMu.RLock()
	resolver := resolvers[path.Base(helperPath)]
	resolversMu.RUnlock()

	if resolver == nil {
		return nil, nil
	}
	return resolver(args)
}

// Run executes helperPath via pkexecPath with args, honoring the global
// dry-run switch and journaling every invocation. It returns the helper's
// stdout and stderr alongside any classified error.
//
// A live invocation whose registered RootCommandResolver refuses is recorded
// as a refusal and returns without dispatching to pkexec, so the journal's
// suppression states stay truthful: "refused" means no root process started,
// and any entry written after dispatch keeps suppression "no" even when the
// helper itself declines, because pkexec did run as root.
func Run(ctx context.Context, pkexecPath, helperPath string, args ...string) (string, string, error) {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}

	rootCmd, resolveErr := resolveRootCommand(helperPath, args)

	if dryrun.Enabled() {
		// Journal Args must reflect the caller's actual inputs, so build the
		// --dry-run-appended argv in a separate slice rather than mutating
		// args (which journalArgs(args) below still reads unmodified).
		dryRunArgs := append(append([]string{}, args...), "--dry-run")
		wouldRun := append([]string{pkexecPath, helperPath}, dryRunArgs...)
		journal.RecordEntry(journal.Entry{
			Action:      action,
			Status:      journal.StatusDryRun,
			Args:        journalArgs(args),
			WouldRun:    wouldRun,
			RootCommand: rootCmd,
			Suppressed:  journal.SuppressedDryRun,
		})
		log.Printf("[DRY-RUN] would execute: %s %s %v", pkexecPath, helperPath, dryRunArgs)
		return "", "", nil
	}

	fullArgs := append([]string{helperPath}, args...)
	wouldRun := append([]string{pkexecPath}, fullArgs...)

	var refusal *RefusalError
	if errors.As(resolveErr, &refusal) {
		journal.RecordEntry(journal.Entry{
			Action:     action,
			Status:     journal.StatusRefused,
			Args:       journalArgs(args),
			WouldRun:   wouldRun,
			Suppressed: journal.SuppressedRefused,
			Error:      refusal.Message,
		})
		log.Printf("refusing %s %s: %s", path.Base(helperPath), action, refusal.Message)
		return "", refusal.Message, &Error{Message: refusal.Message}
	}

	// Record attempt before dispatch
	journal.RecordEntry(journal.Entry{
		Action:      action,
		Status:      journal.StatusAttempt,
		Args:        journalArgs(args),
		WouldRun:    wouldRun,
		RootCommand: rootCmd,
		Suppressed:  journal.SuppressedNone,
	})

	cmd := exec.CommandContext(ctx, pkexecPath, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if stderr.Len() > 0 {
		log.Printf("%s stderr: %s", path.Base(helperPath), stderr.String())
	}

	if err != nil {
		var classifiedErr error
		status := journal.StatusFailure
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = journal.StatusTimeout
			classifiedErr = &Error{Message: "command timed out"}
		} else {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 126 {
				status = journal.StatusDenied
			}
			classifiedErr = classifyFailure(err, helperPath, stderr.String())
		}
		// Suppression stays SuppressedNone for every post-dispatch outcome:
		// pkexec was spawned, so the audit trail must not claim otherwise,
		// however the helper's own stderr reads.
		journal.RecordEntry(journal.Entry{
			Action:      action,
			Status:      status,
			Args:        journalArgs(args),
			WouldRun:    wouldRun,
			RootCommand: rootCmd,
			Suppressed:  journal.SuppressedNone,
			Error:       classifiedErr.Error(),
		})
		return "", stderr.String(), classifiedErr
	}

	// Successful execution
	journal.RecordEntry(journal.Entry{
		Action:      action,
		Status:      journal.StatusSuccess,
		Args:        journalArgs(args),
		WouldRun:    wouldRun,
		RootCommand: rootCmd,
		Suppressed:  journal.SuppressedNone,
	})

	return stdout.String(), stderr.String(), nil
}

// classifyFailure maps a failed invocation's error onto the shared taxonomy:
// *NotFoundError when the process never started because pkexec (or the
// helper) is absent, *Error otherwise. It uses errors.As rather than the
// pre-extraction comma-ok assertions internal/updex carried, so a wrapped
// *exec.Error or *exec.ExitError still classifies instead of falling through
// to the generic *Error. os/exec returns these types unwrapped today, so the
// two forms agree on every error Run can currently observe; the
// wrap-transparency is the drift this package exists to prevent, and
// TestClassifyFailureSeesThroughWrappedErrors pins it.
func classifyFailure(err error, helperPath, stderr string) error {
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return &NotFoundError{Message: fmt.Sprintf("pkexec or %s not found", path.Base(helperPath))}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &Error{
			Message: fmt.Sprintf("command failed (exit %d): %s", exitErr.ExitCode(), stderr),
		}
	}
	return &Error{Message: err.Error()}
}
