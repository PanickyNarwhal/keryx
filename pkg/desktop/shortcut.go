package desktop

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"keryx/pkg/config"
)

type IntegrationStatus struct {
	DesktopEntryPath string `json:"desktopEntryPath"`
	LauncherPath     string `json:"launcherPath"`
	IconPath         string `json:"iconPath"`
	BinaryPath       string `json:"binaryPath"`
	Installed        bool   `json:"installed"`
}

// GetStatus checks whether the Linux .desktop shortcuts and icons are installed.
func GetStatus(cfg *config.Manager) IntegrationStatus {
	home, _ := os.UserHomeDir()
	appsDir := filepath.Join(home, ".local", "share", "applications")
	proclaimDesktop := filepath.Join(appsDir, "Faithlife-Proclaim.desktop")
	keryxDesktop := filepath.Join(appsDir, "Keryx.desktop")
	iconPath := filepath.Join(cfg.DataDir, "proclaim.png")

	_, err1 := os.Stat(proclaimDesktop)
	_, err2 := os.Stat(iconPath)

	binPath, _ := os.Executable()
	localBin := filepath.Join(home, ".local", "bin", "keryx")
	if _, err := os.Stat(localBin); err == nil {
		binPath = localBin
	}

	return IntegrationStatus{
		DesktopEntryPath: proclaimDesktop,
		LauncherPath:     keryxDesktop,
		IconPath:         iconPath,
		BinaryPath:       binPath,
		Installed:        err1 == nil && err2 == nil,
	}
}

// InstallShortcuts installs the Proclaim icon, copies keryx to ~/.local/bin/keryx,
// creates Faithlife-Proclaim.desktop and Keryx.desktop, and registers proclaim:// OAuth URI handling.
func InstallShortcuts(cfg *config.Manager, iconBytes []byte, logf func(string, ...any)) (*IntegrationStatus, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	// 1. Save icon in DataDir and XDG hicolor icon theme
	iconPath := filepath.Join(cfg.DataDir, "proclaim.png")
	if err := os.MkdirAll(filepath.Dir(iconPath), 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(iconPath, iconBytes, 0644); err != nil {
		return nil, err
	}

	hicolorIcon := filepath.Join(home, ".local", "share", "icons", "hicolor", "128x128", "apps", "proclaim.png")
	if err := os.MkdirAll(filepath.Dir(hicolorIcon), 0755); err == nil {
		_ = os.WriteFile(hicolorIcon, iconBytes, 0644)
	}

	// 2. Ensure keryx binary is installed in ~/.local/bin/keryx on Linux
	selfExe, err := os.Executable()
	if err != nil {
		selfExe = "keryx"
	} else if resolved, err := filepath.EvalSymlinks(selfExe); err == nil {
		selfExe = resolved
	}

	targetBin := selfExe
	if runtime.GOOS == "linux" {
		localBinDir := filepath.Join(home, ".local", "bin")
		if err := os.MkdirAll(localBinDir, 0755); err == nil {
			localBinPath := filepath.Join(localBinDir, "keryx")
			if selfExe != localBinPath {
				if copyErr := copyExecutable(selfExe, localBinPath); copyErr == nil {
					targetBin = localBinPath
					if logf != nil {
						logf("Installed Keryx binary to %s", targetBin)
					}
				}
			}
		}
	}

	// 3. Write Faithlife-Proclaim.desktop (direct seamless launch + proclaim:// OAuth handler)
	appsDir := filepath.Join(home, ".local", "share", "applications")
	if err := os.MkdirAll(appsDir, 0755); err != nil {
		return nil, err
	}

	proclaimDesktopPath := filepath.Join(appsDir, "Faithlife-Proclaim.desktop")
	proclaimDesktopContent := fmt.Sprintf(`[Desktop Entry]
Name=Faithlife Proclaim
Comment=Church Presentation Software (via Keryx & Proton-GE)
Exec="%s" launch %%u
Terminal=false
Type=Application
Icon=%s
StartupWMClass=Proclaim.exe
Categories=AudioVideo;Presentation;
MimeType=x-scheme-handler/proclaim;x-scheme-handler/libronixdls;
`, targetBin, iconPath)

	if err := os.WriteFile(proclaimDesktopPath, []byte(proclaimDesktopContent), 0755); err != nil {
		return nil, err
	}

	// 4. Write Keryx.desktop (opens the Keryx Launcher & Setup Manager GUI)
	keryxDesktopPath := filepath.Join(appsDir, "Keryx.desktop")
	keryxDesktopContent := fmt.Sprintf(`[Desktop Entry]
Name=Keryx (Proclaim Launcher)
Comment=Install, Update, and Configure Faithlife Proclaim on Linux
Exec="%s"
Terminal=false
Type=Application
Icon=%s
Categories=Utility;AudioVideo;
`, targetBin, iconPath)

	if err := os.WriteFile(keryxDesktopPath, []byte(keryxDesktopContent), 0755); err != nil {
		return nil, err
	}

	// 5. Register x-scheme-handler/proclaim with xdg-mime and update desktop database on Linux
	if runtime.GOOS == "linux" {
		_ = exec.Command("xdg-mime", "default", "Faithlife-Proclaim.desktop", "x-scheme-handler/proclaim").Run()
		_ = exec.Command("xdg-mime", "default", "Faithlife-Proclaim.desktop", "x-scheme-handler/libronixdls").Run()
		_ = exec.Command("update-desktop-database", appsDir).Run()
		if logf != nil {
			logf("Registered Faithlife-Proclaim.desktop and proclaim:// OAuth protocol handler.")
		}
	}

	st := GetStatus(cfg)
	return &st, nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
