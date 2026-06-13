#!/usr/bin/env sh
# install.sh — build the Tracebox CLI and put it on your PATH.
#
# Builds cmd/tracebox-cli into a binary named `tracebox` and installs it so you
# can run `tracebox run script.py` from any directory. Run it from anywhere —
# it always builds from the repo root.
#
# This installs only the CLI client. The sandbox API server still needs to be
# running separately (see the repo README: `docker compose up -d --build`).

set -eu

# --- Locate the repo root (the directory containing this script). ----------
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$SCRIPT_DIR"

BINARY="tracebox"

# --- Check prerequisites. --------------------------------------------------
if ! command -v go >/dev/null 2>&1; then
	echo "Error: Go is not installed (or not on your PATH)." >&2
	echo "Install Go from https://go.dev/dl/ and try again." >&2
	exit 1
fi

# --- Build. ----------------------------------------------------------------
echo "Building $BINARY from $SCRIPT_DIR ..."
go build -o "$BINARY" ./cmd/tracebox-cli
echo "Built ./$BINARY"

# --- Install onto PATH. ----------------------------------------------------
# Prefer a standard system location if we can write to it; otherwise fall back
# to ~/.local/bin, and finally print manual instructions.
SRC="$SCRIPT_DIR/$BINARY"
INSTALLED=""

try_install() {
	dest_dir="$1"
	if [ -d "$dest_dir" ] && [ -w "$dest_dir" ]; then
		cp "$SRC" "$dest_dir/$BINARY"
		chmod +x "$dest_dir/$BINARY"
		INSTALLED="$dest_dir/$BINARY"
		return 0
	fi
	return 1
}

if try_install "/usr/local/bin"; then
	:
elif try_install "$HOME/.local/bin"; then
	:
else
	# Could not write to a standard bin dir without elevation.
	echo
	echo "Could not copy $BINARY to a directory on your PATH automatically."
	echo
	echo "To finish, either copy it with sudo:"
	echo "    sudo cp \"$SRC\" /usr/local/bin/$BINARY"
	echo
	echo "or add this directory to your PATH (add the line to your shell profile,"
	echo "e.g. ~/.bashrc or ~/.zshrc):"
	echo "    export PATH=\"$SCRIPT_DIR:\$PATH\""
	echo
	echo "Then restart your terminal. After that you can run:"
	echo "    tracebox run script.py"
	exit 0
fi

# --- Success. --------------------------------------------------------------
echo
echo "Installed: $INSTALLED"

# Warn if the chosen dir isn't actually on PATH yet (common for ~/.local/bin).
INSTALL_DIR=$(dirname "$INSTALLED")
case ":$PATH:" in
	*":$INSTALL_DIR:"*) ON_PATH=1 ;;
	*) ON_PATH=0 ;;
esac

if [ "$ON_PATH" -eq 0 ]; then
	echo
	echo "Note: $INSTALL_DIR is not on your PATH yet. Add it (in ~/.bashrc or"
	echo "~/.zshrc) and restart your terminal:"
	echo "    export PATH=\"$INSTALL_DIR:\$PATH\""
fi

echo
echo "Done! You can now run Tracebox from any directory:"
echo "    tracebox run script.py"
if [ "$ON_PATH" -eq 0 ]; then
	echo "(after restarting your terminal so the new PATH takes effect)"
fi
