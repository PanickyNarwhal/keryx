# Keryx

> "Preach (**proclaim**) the word; be ready in season and out of season; correct, rebuke, and encourage with great patience and teaching."
> (**κήρυξον** τὸν λόγον — from **κῆρυξ**, *herald / proclaimer*)
> — 2 Timothy 4:2 [CSB](https://www.biblegateway.com/passage/?search=2+Timothy+4%3A2&version=CSB)

**Keryx** is a standalone Linux installer and manager for **Faithlife Proclaim** church presentation software, built on [Matheteusate](https://github.com/panickynarwhal/Matheteusate) and [Ou Dedetai](https://github.com/FaithLife-Community/OuDedetai), maintained by Isaac Gerling and licensed under the MIT License.

## What this is
Keryx is a Python application to install, update, and run **Faithlife Proclaim** on Linux (and in the future BSD) through the **Proton-GE** compatibility layer.

## Why Keryx + Proton-GE Works for Proclaim
* **Direct 64-bit MSI Installation**: Bypasses the legacy `ProclaimSetup.exe` web bootstrapper (which requires Windows `TrustedInstaller` elevation and fails in Wine) and fetches `Proclaim-x64.msi` directly from Faithlife's `proclaim` release feed.
* **Self-Contained .NET 10 + ICU**: Automatically provisions Windows `icu` globalization data (`icu.dll` / `icudtl.dat`) required by `Proclaim-x64.msi`'s `HASICUDLL` launch check, running Proclaim's bundled self-contained `.NET 10` and `LogosCEF` runtime without needing `winetricks dotnet48` or Wine-Mono.
* **Direct3D 11 & Media Foundation via Proton-GE**: Proclaim relies on `SharpDX.Direct3D11`, `Vortice.Direct3D11`, `Vortice.MediaFoundation`, and `EVRPresenter.dll` for hardware-accelerated slide transitions, motion backgrounds, and video playback. Proton-GE provides built-in **DXVK** (Direct3D 9/11 over Vulkan) and patched **Media Foundation (`mfplat`) / GStreamer** support out of the box.
* **OAuth Browser Sign-In**: Registers the `proclaim:` (`x-scheme-handler/proclaim`) desktop URI handler so signing into Faithlife in your Linux browser redirects straight back into Proclaim.
* **Standalone Operation**: Manages the GE-Proton runtime automatically in `data/bin` and `data/pfx`—no Steam installation required.

---
In excelsis Deo
