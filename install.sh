#!/usr/bin/env bash

# Keryx Bootstrapper
# This script sets up a virtual environment, installs dependencies

set -e

if [ ! -d "keryx" ]; then
    echo "Error: Please run this script from the root of the Keryx project directory."
    exit 1
fi

echo "--- Setting up Keryx ---"

if ! command -v python3 &> /dev/null; then
    echo "Error: Python 3 is required but not found."
    exit 1
fi

if [ ! -d "venv" ]; then
    echo "Creating virtual environment..."
    python3 -m venv venv
fi

echo "Installing/Updating dependencies..."
# We use 'python -m pip' to ensure we use the venv's pip
./venv/bin/python3 -m pip install --upgrade pip
./venv/bin/python3 -m pip install distro packaging psutil pythondialog requests inotify

echo "--- Setup Complete ---"
echo "Launching Keryx in TUI mode..."
echo "Note: If you are using the Fish shell, you can later run 'source venv/bin/activate.fish' to enter the environment manually."
## it was very stubborn in CachyOS but ran fine on Garuda
env DIALOG=curses ./venv/bin/python3 -m keryx.main "$@"
