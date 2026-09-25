#!/usr/bin/env bash
# Drive ChairLift through Maintenance page cleanup and recovery confirmation
# dialogs under AT-SPI with --dry-run and record observations.
#
# Usage: run_maintenance_atspi.sh <chairlift-binary> <output-dir>
#
# Writes <output-dir>/atspi-results.txt (the probe's records),
# chairlift.log (the application's output) and probe.log (the probe's
# stderr).
set -euo pipefail

APP="${1:?usage: run_maintenance_atspi.sh <chairlift-binary> <output-dir>}"
OUTDIR="${2:?usage: run_maintenance_atspi.sh <chairlift-binary> <output-dir>}"

mkdir -p "$OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd)"

WIDTH="${CHAIRLIFT_ATSPI_WIDTH:-1400}"
HEIGHT="${CHAIRLIFT_ATSPI_HEIGHT:-900}"
DISPLAY_NUM="${CHAIRLIFT_ATSPI_DISPLAY:-:97}"

LOG="$OUTDIR/chairlift.log"
PROBE_LOG="$OUTDIR/probe.log"
RESULTS="$OUTDIR/atspi-results.txt"
: > "$LOG"
: > "$PROBE_LOG"
: > "$RESULTS"

cleanup() {
    if [ -n "${XVFB_PID:-}" ]; then
        kill "$XVFB_PID" 2>/dev/null || true
        wait "$XVFB_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

Xvfb "$DISPLAY_NUM" -screen 0 "${WIDTH}x${HEIGHT}x24" -nolisten tcp &
XVFB_PID=$!
export DISPLAY="$DISPLAY_NUM"

# Wait for the display to accept clients rather than sleeping a fixed time.
for _ in $(seq 1 100); do
    if xdpyinfo >/dev/null 2>&1; then break; fi
    sleep 0.1
done
xdpyinfo >/dev/null 2>&1 || { echo "Xvfb on $DISPLAY_NUM never became ready" >&2; exit 1; }

export LANG=C LC_ALL=C GSETTINGS_BACKEND=memory GDK_DEBUG=no-portals
export GTK_A11Y=atspi
unset NO_AT_BRIDGE || true

if [ -n "${CHAIRLIFT_SCHEMA_DIR:-}" ]; then
  export GSETTINGS_SCHEMA_DIR="$CHAIRLIFT_SCHEMA_DIR"
fi
export HOME="$OUTDIR/home"
export XDG_RUNTIME_DIR="$OUTDIR/runtime"
mkdir -p "$HOME"
mkdir -p "$XDG_RUNTIME_DIR"
chmod 0700 "$XDG_RUNTIME_DIR"

# Provide stub binaries for flatpak and brew in a private bin directory so
# cleanup providers are recognized deterministically.
BINDIR="$OUTDIR/bin"
mkdir -p "$BINDIR"
cat << 'EOF' > "$BINDIR/flatpak"
#!/bin/sh
case "${1:-}" in
    --version)
        echo "Flatpak 1.14.4"
        exit 0
        ;;
    uninstall)
        echo "Nothing unused to uninstall"
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
EOF
chmod +x "$BINDIR/flatpak"

cat << 'EOF' > "$BINDIR/brew"
#!/bin/sh
case "${1:-}" in
    --version)
        echo "Homebrew 4.2.0"
        exit 0
        ;;
    cleanup)
        echo "Homebrew cache cleaned"
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
EOF
chmod +x "$BINDIR/brew"

export PATH="$BINDIR:$PATH"

# Stage an isolated binary copy into OUTDIR so config.dev.yml can live beside
# it without modifying the build directory or being shadowed by /etc or cwd.
cp "$APP" "$OUTDIR/chairlift"
chmod +x "$OUTDIR/chairlift"
cat << 'EOF' > "$OUTDIR/config.dev.yml"
maintenance_page:
  maintenance_freespace_group:
    enabled: true
  reset_group:
    enabled: true
EOF
APP="$OUTDIR/chairlift"

PROBE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/maintenance_atspi_probe.py"
[ -r "$PROBE" ] || { echo "probe $PROBE is missing" >&2; exit 1; }

export CHAIRLIFT_ATSPI_APP="$APP"
export CHAIRLIFT_ATSPI_LOG="$LOG"
export CHAIRLIFT_ATSPI_PROBE="$PROBE"
export CHAIRLIFT_ATSPI_PROBE_LOG="$PROBE_LOG"
export CHAIRLIFT_ATSPI_RESULTS="$RESULTS"
export CHAIRLIFT_ATSPI_OUTDIR="$OUTDIR"

# shellcheck disable=SC2016
dbus-run-session -- bash -eu -o pipefail -c '
    cd "$CHAIRLIFT_ATSPI_OUTDIR"
    "$CHAIRLIFT_ATSPI_APP" --dry-run >>"$CHAIRLIFT_ATSPI_LOG" 2>&1 &
    APP_PID=$!

    finish() {
        kill -TERM "$APP_PID" 2>/dev/null || true
        wait "$APP_PID" 2>/dev/null || true
    }
    trap finish EXIT

    ready=0
    for _ in $(seq 1 300); do
        if grep -q "Running in dry-run mode" "$CHAIRLIFT_ATSPI_LOG" \
            && grep -q "ChairLift activated" "$CHAIRLIFT_ATSPI_LOG" \
            && grep -q "app: window presented" "$CHAIRLIFT_ATSPI_LOG" \
            && grep -q "views: reset group built" "$CHAIRLIFT_ATSPI_LOG"; then
            ready=1
            break
        fi
        if ! kill -0 "$APP_PID" 2>/dev/null; then
            echo "ChairLift exited before becoming ready:" >&2
            cat "$CHAIRLIFT_ATSPI_LOG" >&2
            exit 1
        fi
        sleep 0.1
    done
    [ "$ready" = 1 ] || { echo "ChairLift did not become ready in 30s:" >&2; cat "$CHAIRLIFT_ATSPI_LOG" >&2; exit 1; }


    python3 "$CHAIRLIFT_ATSPI_PROBE" "$CHAIRLIFT_ATSPI_LOG" \
        >"$CHAIRLIFT_ATSPI_RESULTS" 2>"$CHAIRLIFT_ATSPI_PROBE_LOG"
'

echo "atspi maintenance probe complete"
