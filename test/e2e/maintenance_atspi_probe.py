#!/usr/bin/env python3
"""Exercise Maintenance page cleanup and recovery confirmation dialogs via AT-SPI.

The probe reports what it observes as tab-separated records. Assertions live
in maintenance_atspi_test.go, alongside pageview contracts.
"""

import sys
import time

try:
    from dogtail import rawinput, tree
    from dogtail.config import config
except ImportError as error:  # pragma: no cover - Go test reports this prerequisite
    print(f"dogtail is not importable: {error}", file=sys.stderr)
    sys.exit(3)

APPLICATION_NAMES = ("chairlift", "Control Center", "io.projectbluefin.chairlift")


def descendants(node):
    """Yield a node and its accessible descendants, tolerating stale proxies."""
    yield node
    try:
        children = node.children
    except Exception:
        return
    for child in children:
        yield from descendants(child)


def name_of(node):
    try:
        return node.name or ""
    except Exception:
        return ""


def role_of(node):
    try:
        return node.roleName or ""
    except Exception:
        return ""


def is_sensitive(node):
    try:
        states = getattr(node, "states", [])
        if states:
            from dogtail.atspi import STATE_SENSITIVE
            return STATE_SENSITIVE in states
    except Exception:
        pass
    try:
        return bool(node.sensitive)
    except Exception:
        pass
    return True


def named_node(root, name, roles=None):
    for node in descendants(root):
        if name_of(node) != name:
            continue
        if roles and role_of(node) not in roles:
            continue
        return node
    return None


def find_button_in(root, name):
    for node in descendants(root):
        if name_of(node) == name and role_of(node) in ("push button", "button"):
            return node
    return None


def find_application():
    for name in APPLICATION_NAMES:
        try:
            return tree.root.application(name)
        except Exception:
            pass
    try:
        published = [name_of(child) for child in tree.root.children]
    except Exception as error:
        raise RuntimeError(f"cannot enumerate accessibility bus: {error}") from error
    raise RuntimeError(
        f"ChairLift is absent from AT-SPI (tried {APPLICATION_NAMES}; bus publishes {published})"
    )


def wait_for(root, name, timeout, roles=None):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        node = named_node(root, name, roles)
        if node is not None:
            return node
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for accessible element {name!r}")


def wait_for_condition(predicate, timeout, desc="condition"):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if predicate():
                return True
        except Exception:
            pass
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for {desc}")


def find_dialog(root, title):
    for node in descendants(root):
        if name_of(node) == title and role_of(node) in ("dialog", "alert", "window"):
            return node
        if role_of(node) in ("dialog", "alert"):
            for child in descendants(node):
                if name_of(child) == title:
                    return node
    return None


def wait_for_dialog(root, title, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        d = find_dialog(root, title)
        if d is not None:
            return d
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for dialog {title!r}")


def wait_for_dialog_dismissal(title, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if find_dialog(tree.root, title) is None:
            return True
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for dialog {title!r} to dismiss")


def dialog_body(dialog):
    texts = []
    excluded = {
        "Cancel",
        "Remove Everything",
        "Factory Reset",
        "Remove Everything You Installed?",
        "Factory Reset This System?",
    }
    for node in descendants(dialog):
        name = name_of(node)
        if name and name not in excluded:
            texts.append(name)
    return " ".join(texts)


def activate(node):
    """Invoke the AT-SPI action, falling back to dogtail's node activation."""
    action = getattr(node, "doActionNamed", None)
    if action is not None:
        for act in ("click", "activate", "press"):
            try:
                action(act)
                return
            except Exception:
                pass
    click = getattr(node, "click", None)
    if click is not None:
        try:
            click()
            return
        except Exception:
            pass
    raise RuntimeError(f"{name_of(node)!r} could not be activated")


def emit(kind, **fields):
    print("\t".join([kind] + [f"{key}={value}" for key, value in fields.items()]), flush=True)


def main():
    config.searchBackoffDuration = 0.25
    config.searchCutoffCount = 20
    config.actionDelay = 0.2
    config.defaultDelay = 0.2
    config.typingDelay = 0.05
    config.logDebugToFile = False
    config.logDebugToStdOut = False

    app = find_application()

    # 1. Navigate to Maintenance destination
    maintenance = wait_for(app, "Maintenance", 30)
    activate(maintenance)
    time.sleep(0.5)
    emit("PAGE", name="Maintenance", selected=int(bool(getattr(maintenance, "selected", False))))

    # 2. Storage Clean Up action button interaction
    cleanup_btn = wait_for(app, "Clean up", 15, roles=("push button", "button"))
    emit("BUTTON", name="Clean up", role=role_of(cleanup_btn), sensitive=int(is_sensitive(cleanup_btn)))
    activate(cleanup_btn)
    emit("STATE", name="Clean up", status="busy")

    wait_for_condition(
        lambda: is_sensitive(cleanup_btn) and name_of(cleanup_btn) == "Clean up",
        30,
        desc="Clean up completion",
    )
    emit("STATE", name="Clean up", status="completed", sensitive=1)

    # 3. Powerwash confirmation flow
    powerwash_btn = wait_for(app, "Remove Everything", 15, roles=("push button", "button"))
    emit("BUTTON", name="Remove Everything", role=role_of(powerwash_btn))

    activate(powerwash_btn)
    pw_dialog = wait_for_dialog(tree.root, "Remove Everything You Installed?", 15)
    pw_body = dialog_body(pw_dialog)
    pw_has_flatpak = int("Flatpak" in pw_body)
    pw_has_undone = int("cannot be undone" in pw_body)
    pw_cancel = find_button_in(pw_dialog, "Cancel")
    pw_confirm = find_button_in(pw_dialog, "Remove Everything")
    emit(
        "DIALOG",
        type="powerwash",
        title="Remove Everything You Installed?",
        has_cancel=int(pw_cancel is not None),
        has_confirm=int(pw_confirm is not None),
        body_valid=int(pw_has_flatpak and pw_has_undone),
    )

    # Cancel dismisses dialog without execution
    activate(pw_cancel)
    wait_for_dialog_dismissal("Remove Everything You Installed?", 10)
    emit("DIALOG_CANCELLED", type="powerwash", dismissed=1)

    # Re-click to confirm
    activate(powerwash_btn)
    pw_dialog = wait_for_dialog(tree.root, "Remove Everything You Installed?", 15)
    pw_confirm = find_button_in(pw_dialog, "Remove Everything")
    activate(pw_confirm)
    wait_for_dialog_dismissal("Remove Everything You Installed?", 10)

    wait_for_condition(
        lambda: is_sensitive(powerwash_btn) and name_of(powerwash_btn) == "Remove Everything",
        30,
        desc="Powerwash completion",
    )
    emit("DIALOG_CONFIRMED", type="powerwash", status="completed")

    # 4. Factory Reset confirmation flow
    reset_btn = wait_for(app, "Factory Reset", 15, roles=("push button", "button"))
    emit("BUTTON", name="Factory Reset", role=role_of(reset_btn))

    activate(reset_btn)
    fr_dialog = wait_for_dialog(tree.root, "Factory Reset This System?", 15)
    fr_body = dialog_body(fr_dialog)
    fr_has_exp = int("--experimental" in fr_body)
    fr_has_undone = int("cannot be undone" in fr_body)
    fr_cancel = find_button_in(fr_dialog, "Cancel")
    fr_confirm = find_button_in(fr_dialog, "Factory Reset")
    emit(
        "DIALOG",
        type="factory_reset",
        title="Factory Reset This System?",
        has_cancel=int(fr_cancel is not None),
        has_confirm=int(fr_confirm is not None),
        body_valid=int(fr_has_exp and fr_has_undone),
    )

    # Cancel dismisses dialog without execution
    activate(fr_cancel)
    wait_for_dialog_dismissal("Factory Reset This System?", 10)
    emit("DIALOG_CANCELLED", type="factory_reset", dismissed=1)

    # Re-click to confirm
    activate(reset_btn)
    fr_dialog = wait_for_dialog(tree.root, "Factory Reset This System?", 15)
    fr_confirm = find_button_in(fr_dialog, "Factory Reset")
    activate(fr_confirm)
    wait_for_dialog_dismissal("Factory Reset This System?", 10)

    wait_for_condition(
        lambda: is_sensitive(reset_btn) and name_of(reset_btn) == "Factory Reset",
        30,
        desc="Factory Reset completion",
    )
    emit("DIALOG_CONFIRMED", type="factory_reset", status="completed")

    emit("DONE")
    return 0


if __name__ == "__main__":
    sys.exit(main())
