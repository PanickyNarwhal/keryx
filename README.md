# Keryx
This project is now depreciated because Proclaim has a Web app which defeats the whole purpose of this.

> "Preach (**proclaim**) the word; be ready in season and out of season; correct, rebuke, and encourage with great patience and teaching."
> (**κήρυξον** τὸν λόγον — from **κῆρυξ**, *herald / proclaimer*)
> — 2 Timothy 4:2 [CSB](https://www.biblegateway.com/passage/?search=2+Timothy+4%3A2&version=CSB)

**Keryx** is a zero-dependency **Go** launcher and guided installer that runs **Faithlife Proclaim** church presentation software on Linux almost natively via **Proton-GE** (or Wine).

---

## How It Works

1. **Direct Official Installer Resolution**:
   - Scrapes [`https://proclaim.logos.com/download-confirmation?platform=win`](https://proclaim.logos.com/download-confirmation?platform=win) to locate the official Windows bootstrapper (`ProclaimSetup.exe`).
   - Inspects the UTF-16LE `ProclaimWebScript.lbxdat` manifest inside `ProclaimSetup.exe` to resolve the exact 64-bit `Proclaim-x64.msi` URL and version, bypassing Windows `BITS` and `TrustedInstaller` elevation issues under Wine.
2. **Automated Proton-GE Compatibility Engine**:
   - Automatically detects existing Proton-GE / Steam Proton installations (`~/.steam/root/compatibilitytools.d`) or downloads and extracts the latest `GE-Proton` release into `~/.local/share/keryx/runners`.
3. **Windows 10 (Build 19045) + ICU 73.2 + DXVK**:
   - Initializes a dedicated 64-bit prefix (`~/.local/share/keryx/prefix/pfx`), sets Windows 10 Build 19045, provisions `icu.dll` / `icudtl.dat` into `system32` (satisfying Proclaim's `HASICUDLL` check and `.NET 10` globalization), and enables **DXVK** (Direct3D 11 over Vulkan) + GStreamer Media Foundation for smooth slide transitions and video playback.
4. **Seamless Native Desktop & OAuth Integration**:
   - Creates `Faithlife-Proclaim.desktop` (`keryx launch %u`) and `Keryx.desktop` in `~/.local/share/applications`.
   - Registers the `proclaim://` (`x-scheme-handler/proclaim`) OAuth URI handler so signing into Faithlife in your Linux browser redirects straight back into Proclaim.

---

## Getting Started

### Build from Source (Go 1.22+)
```bash
git clone https://github.com/PanickyNarwhal/keryx.git
cd keryx
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o keryx .
```

### Usage
```bash
# Open the Keryx Desktop Launcher & Guided 5-Step Setup Walkthrough UI
./keryx

# Launch Faithlife Proclaim directly (also used by the desktop shortcut & proclaim:// OAuth links)
./keryx launch

# Run the guided 5-step setup walkthrough in the terminal
./keryx setup

# Check Proclaim release status, installed version, and Proton-GE runner diagnostics
./keryx status
```

---
In excelsis Deo
