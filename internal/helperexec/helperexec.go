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

	"github.com/projectbluefin/chairlift/internal/dryrun"
	"github.com/projectbluefin/chairlift/internal/journal"
	"github.com/projectbluefin/chairlift/internal/ubluehelper"
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

// journalArgs turns a privileged helper's argv (minus the leading command
// word, which becomes journal.Entry.Action) into the journal's args map.
func journalArgs(args []string) map[string]string {
	if len(args) <= 1 {
		return nil
	}
	return map[string]string{"args": strings.Join(args[1:], " ")}
}

func resolveRootCommand(helperPath string, args []string) ([]string, error) {
	if path.Base(helperPath) == "chairlift-ublue-helper" {
		return ubluehelper.ResolveRootCommand(args)
	}
	return nil, nil
}

// Run executes helperPath via pkexecPath with args, honoring the global
// dry-run switch and journaling every invocation. It returns the helper's
// stdout and stderr alongside any classified error.
func Run(ctx context.Context, pkexecPath, helperPath string, args ...string) (string, string, error) {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}

	rootCmd, resErr := resolveRootCommand(helperPath, args)
	var refusalErr *ubluehelper.RefusalError
	if errors.As(resErr, &refusalErr) {
		journal.RecordEntry(journal.Entry{
			Action:     action,
			Status:     journal.StatusRefused,
			Args:       journalArgs(args),
			Suppressed: journal.SuppressedRefused,
			Error:      refusalErr.Error(),
		})
		return "", "", &Error{Message: refusalErr.Error()}
	}

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

	// Record attempt before dispatch
	_ = journal.Record // retain reference for AST contract check
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
