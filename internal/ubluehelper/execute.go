package ubluehelper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// Runner executes one privileged command, forwarding its output to the
// helper's console. When stderr is non-nil the command's standard error is
// also teed into it so the caller can inspect what the command said. It is
// the signature of cmd/chairlift-helper's runCapture; tests substitute a
// fake.
type Runner func(ctx context.Context, stderr io.Writer, name string, args ...string) error

// RollbackRefLookup reports the image reference of the host's rollback
// deployment, or "" when there is none. cmd/chairlift-helper wires it to
// bootc.GetStatus.
type RollbackRefLookup func(ctx context.Context) (string, error)

// GroupEnsurer makes group exist in /etc/group so a following usermod can add
// a member to it. cmd/chairlift-helper wires it to EnsureLocalGroup with the
// real group-file paths.
type GroupEnsurer func(group string, dryRun bool) error

// SwitchImage runs a derived `bootc switch` argv and returns the line the
// helper prints on success. Switching back to the image the host just left is
// a switch to the rollback deployment, which a composefs `bootc switch`
// refuses ("Target image has the same fs-verity digest as the existing
// Some(Rollback) deployment"); making that deployment the next boot with the
// fixed rollback argv is the switch the person asked for, so when the refusal
// is seen and the rollback deployment is the requested target, SwitchImage
// runs `bootc rollback` instead. Any other failure, and a rollback that fails
// in turn, is returned as the error text the helper prints before exiting
// non-zero, prefixed with failure.
func SwitchImage(ctx context.Context, run Runner, rollbackRef RollbackRefLookup, args []string, failure string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("%s: empty bootc argv", failure)
	}
	target := args[len(args)-1]
	var captured bytes.Buffer
	err := run(ctx, &captured, "bootc", args...)
	if err == nil {
		return fmt.Sprintf("switched to %s — restart to apply", target), nil
	}
	if IsSameRollbackRefusal(captured.String()) {
		if ref, lookupErr := rollbackRef(ctx); lookupErr == nil && ref == target {
			if rerr := run(ctx, nil, "bootc", RollbackArgs()...); rerr != nil {
				return "", fmt.Errorf("%s: %v; rollback to %s failed: %v", failure, err, target, rerr)
			}
			return fmt.Sprintf("switched to %s (the previous deployment) — restart to apply", target), nil
		}
	}
	return "", fmt.Errorf("%s: %v", failure, err)
}

// ErrNoDevGroupsChanged is returned by ApplyDevGroups when every developer
// group was skipped.
var ErrNoDevGroupsChanged = errors.New("no developer groups could be changed")

// ApplyDevGroups adds or removes username across every developer group and
// returns how many groups it changed. A group that does not exist on this
// image is reported on stderr with SkippedGroupLine and skipped rather than
// failing the whole operation: the group set is shared across Bluefin,
// Bluefin LTS, and Dakota, which do not all ship the same ones. Enabling
// first runs ensure for each group so an image-only group gets a local
// /etc/group entry. A dry run prints each argv to stdout instead of running
// it and counts it as applied. The error is ErrNoDevGroupsChanged when no
// group could be changed, and a refusal when command is not a dx command.
func ApplyDevGroups(ctx context.Context, run Runner, ensure GroupEnsurer, command, username string, dryRun bool, stdout, stderr io.Writer) (int, error) {
	applied := 0
	for _, group := range DevGroups() {
		name, args, ok := GroupArgs(command, username, group)
		if !ok {
			return applied, fmt.Errorf("unsupported group operation for command %q", command)
		}

		if command == CommandDXEnable {
			if err := ensure(group, dryRun); err != nil {
				_, _ = fmt.Fprintln(stderr, SkippedGroupLine(group, err))
				continue
			}
		}

		if dryRun {
			_, _ = fmt.Fprintf(stdout, "[DRY-RUN] would execute: %s %v\n", name, args)
			applied++
			continue
		}

		if err := run(ctx, nil, name, args...); err != nil {
			// usermod/gpasswd fail on a group this image does not define;
			// report it and carry on with the rest.
			_, _ = fmt.Fprintln(stderr, SkippedGroupLine(group, err))
			continue
		}
		applied++
	}

	if applied == 0 {
		return 0, ErrNoDevGroupsChanged
	}
	return applied, nil
}

// DevGroupsAppliedLine is the line the helper prints after ApplyDevGroups
// changed applied groups for username.
func DevGroupsAppliedLine(command string, applied int, username string) string {
	return fmt.Sprintf("%s applied to %d group(s) for %s — log out and back in to take effect",
		command, applied, username)
}
