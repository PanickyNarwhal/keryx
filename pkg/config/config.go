package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

const (
	AppName    = "Keryx"
	AppVersion = "1.0.0"

	// Official Faithlife Proclaim Windows download confirmation page
	ProclaimDownloadConfirmationURL = "https://proclaim.logos.com/download-confirmation?platform=win"
	// Fallback direct bootstrapper URL
	ProclaimBootstrapFallbackURL = "https://downloads.logoscdn.com/Proclaim/Installer/ProclaimSetup.exe"
	// Fallback Atom/XML feed URL
	ProclaimFeedFallbackURL = "https://clientservices.logos.com/update/v1/feed/proclaim/stable.xml"

	// Proton-GE GitHub API & fallback
	ProtonGELatestAPIURL    = "https://api.github.com/repos/GloriousEggroll/proton-ge-custom/releases/latest"
	ProtonGEFallbackTag     = "GE-Proton10-32"
	ProtonGEFallbackURL     = "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton10-32/GE-Proton10-32.tar.gz"
	ICUWinFallbackURL       = "https://github.com/FaithLife-Community/icu/releases/download/72.1-custom%2B4/icu-win.tar.gz"
	ProclaimUpgradeCode     = "{500E8D69-8AC5-4226-B110-A4B13B537DD0}"
)

type Settings struct {
	RunnerMode         string `json:"runnerMode"`          // "proton-ge", "steam-proton", "system-wine"
	SelectedRunnerName string `json:"selectedRunnerName"`  // e.g., "GE-Proton10-32"
	SelectedWine64Path string `json:"selectedWine64Path"`  // resolved path to wine64/wine
	EnableDXVK         bool   `json:"useExperimentalDxvk"` // use DXVK Vulkan backend instead of WineD3D/GDI
	EnableEsync        bool   `json:"enableEsync"`         // WINEESYNC=1
	EnableFsync        bool   `json:"enableFsync"`         // WINEFSYNC=1
	UseMsiQuietMode    bool   `json:"useMsiQuietMode"`     // false = /passive, true = /qn
	VirtualDesktop     string `json:"virtualDesktop"`      // e.g. "" (off) or "1920x1080"
	InstalledVersion   string `json:"installedVersion"`    // cached installed version
	LastSetupCompleted bool   `json:"lastSetupCompleted"`  // true once walkthrough finishes
}

type Manager struct {
	mu         sync.RWMutex
	ConfigDir  string
	ConfigFile string
	DataDir    string
	RunnersDir string
	PrefixDir  string
	CacheDir   string
	LogsDir    string
	Settings   Settings
}

func NewManager() (*Manager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "."
	}

	configBase := os.Getenv("XDG_CONFIG_HOME")
	if configBase == "" {
		if runtime.GOOS == "windows" {
			configBase = filepath.Join(homeDir, "AppData", "Roaming")
		} else {
			configBase = filepath.Join(homeDir, ".config")
		}
	}

	dataBase := os.Getenv("KERYX_HOME")
	if dataBase == "" {
		xdgData := os.Getenv("XDG_DATA_HOME")
		if xdgData != "" {
			dataBase = filepath.Join(xdgData, "keryx")
		} else if runtime.GOOS == "windows" {
			dataBase = filepath.Join(homeDir, "AppData", "Local", "keryx")
		} else {
			dataBase = filepath.Join(homeDir, ".local", "share", "keryx")
		}
	}

	cfgDir := filepath.Join(configBase, "keryx")
	m := &Manager{
		ConfigDir:  cfgDir,
		ConfigFile: filepath.Join(cfgDir, "config.json"),
		DataDir:    dataBase,
		RunnersDir: filepath.Join(dataBase, "runners"),
		PrefixDir:  filepath.Join(dataBase, "prefix", "pfx"),
		CacheDir:   filepath.Join(dataBase, "cache"),
		LogsDir:    filepath.Join(dataBase, "logs"),
		Settings: Settings{
			RunnerMode:      "proton-ge",
			EnableDXVK:      false,
			EnableEsync:     true,
			EnableFsync:     true,
			UseMsiQuietMode: false,
		},
	}

	if err := m.EnsureDirs(); err != nil {
		return nil, err
	}
	_ = m.Load()
	return m, nil
}

func (m *Manager) EnsureDirs() error {
	dirs := []string{
		m.ConfigDir,
		m.DataDir,
		m.RunnersDir,
		m.PrefixDir,
		m.CacheDir,
		m.LogsDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.ConfigFile)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.Settings)
}

func (m *Manager) Save() error {
	m.mu.RLock()
	data, err := json.MarshalIndent(m.Settings, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.ConfigDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(m.ConfigFile, data, 0644)
}

func (m *Manager) GetSettings() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Settings
}

func (m *Manager) UpdateSettings(fn func(s *Settings)) error {
	m.mu.Lock()
	fn(&m.Settings)
	m.mu.Unlock()
	return m.Save()
}
