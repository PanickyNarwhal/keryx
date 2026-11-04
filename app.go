package main

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"keryx/pkg/config"
	"keryx/pkg/desktop"
	"keryx/pkg/installer"
	"keryx/pkg/proton"
)

type SetupStep struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Status      string  `json:"status"` // "pending", "running", "done", "error"
	Detail      string  `json:"detail"`
	Percent     float64 `json:"percent"`
}

type AppState struct {
	AppName           string                    `json:"appName"`
	AppVersion        string                    `json:"appVersion"`
	HostOS            string                    `json:"hostOs"`
	HostArch          string                    `json:"hostArch"`
	IsInstalled       bool                      `json:"isInstalled"`
	IsRunning         bool                      `json:"isRunning"`
	InstalledVersion  string                    `json:"installedVersion"`
	ProclaimExePath   string                    `json:"proclaimExePath"`
	LatestRelease     *installer.ReleaseInfo    `json:"latestRelease"`
	UpdateAvailable   bool                      `json:"updateAvailable"`
	ActiveRunner      *proton.Runner            `json:"activeRunner"`
	AvailableRunners  []proton.Runner           `json:"availableRunners"`
	Settings          config.Settings           `json:"settings"`
	DesktopStatus     desktop.IntegrationStatus `json:"desktopStatus"`
	DataDir           string                    `json:"dataDir"`
	PrefixDir         string                    `json:"prefixDir"`
	SetupInProgress   bool                      `json:"setupInProgress"`
	CurrentStep       int                       `json:"currentStep"`
	DownloadMBps      float64                   `json:"downloadMbps"`
	DownloadPercent   float64                   `json:"downloadPercent"`
	Steps             []SetupStep               `json:"steps"`
	Logs              []string                  `json:"logs"`
	LastError         string                    `json:"lastError"`
}

type App struct {
	mu              sync.RWMutex
	cfg             *config.Manager
	iconBytes       []byte
	latestRelease   *installer.ReleaseInfo
	setupInProgress bool
	currentStep     int
	downloadMBps    float64
	downloadPercent float64
	steps           []SetupStep
	logs            []string
	lastError       string
}

func NewApp(cfg *config.Manager, iconBytes []byte) *App {
	a := &App{
		cfg:       cfg,
		iconBytes: iconBytes,
		steps:     defaultSetupSteps(),
	}
	a.Logf("Keryx %s initialized (DataDir: %s)", config.AppVersion, cfg.DataDir)
	return a
}

func defaultSetupSteps() []SetupStep {
	return []SetupStep{
		{
			ID:          1,
			Title:       "Environment & Prerequisite Check",
			Description: "Verify Linux environment, storage directories, and display server.",
			Status:      "pending",
		},
		{
			ID:          2,
			Title:       "Proton-GE Compatibility Engine",
			Description: "Detect existing Proton-GE / Steam Proton or download the latest GE-Proton release.",
			Status:      "pending",
		},
		{
			ID:          3,
			Title:       "Windows 10 Prefix, ICU & DXVK",
			Description: "Initialize 64-bit prefix, Windows 10 (Build 19045), ICU globalization (HASICUDLL), and Direct3D 11 DXVK.",
			Status:      "pending",
		},
		{
			ID:          4,
			Title:       "Download & Install Faithlife Proclaim",
			Description: "Fetch installer from proclaim.logos.com/download-confirmation?platform=win and install Proclaim-x64.msi.",
			Status:      "pending",
		},
		{
			ID:          5,
			Title:       "Desktop Shortcuts & OAuth Sign-In",
			Description: "Create native Linux application launchers and register the proclaim:// OAuth protocol handler.",
			Status:      "pending",
		},
	}
}

func (a *App) Logf(format string, args ...any) {
	msg := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	fmt.Println(msg)
	a.mu.Lock()
	a.logs = append(a.logs, msg)
	if len(a.logs) > 250 {
		a.logs = a.logs[len(a.logs)-250:]
	}
	a.mu.Unlock()
}

func (a *App) GetState() AppState {
	exePath := proton.FindProclaimExe(a.cfg)
	installedVer := proton.GetInstalledVersion(a.cfg)
	runners := proton.DiscoverRunners(a.cfg)
	activeRunner := proton.GetActiveRunner(a.cfg)
	desktopSt := desktop.GetStatus(a.cfg)
	isRunning := proton.IsProclaimRunning()

	a.mu.RLock()
	defer a.mu.RUnlock()

	stepsCopy := make([]SetupStep, len(a.steps))
	copy(stepsCopy, a.steps)

	logsCopy := make([]string, len(a.logs))
	copy(logsCopy, a.logs)

	updateAvail := false
	if exePath != "" && a.latestRelease != nil && a.latestRelease.Version != "" && installedVer != "" && installedVer != "Installed" {
		updateAvail = installedVer != a.latestRelease.Version
	}

	return AppState{
		AppName:          config.AppName,
		AppVersion:       config.AppVersion,
		HostOS:           runtime.GOOS,
		HostArch:         runtime.GOARCH,
		IsInstalled:      exePath != "",
		IsRunning:        isRunning,
		InstalledVersion: installedVer,
		ProclaimExePath:  exePath,
		LatestRelease:    a.latestRelease,
		UpdateAvailable:  updateAvail,
		ActiveRunner:     activeRunner,
		AvailableRunners: runners,
		Settings:         a.cfg.GetSettings(),
		DesktopStatus:    desktopSt,
		DataDir:          a.cfg.DataDir,
		PrefixDir:        a.cfg.PrefixDir,
		SetupInProgress:  a.setupInProgress,
		CurrentStep:      a.currentStep,
		DownloadMBps:     a.downloadMBps,
		DownloadPercent:  a.downloadPercent,
		Steps:            stepsCopy,
		Logs:             logsCopy,
		LastError:        a.lastError,
	}
}

func (a *App) CheckForUpdates(ctx context.Context) (*installer.ReleaseInfo, error) {
	rel, err := installer.ResolveFromDownloadConfirmation(ctx, a.cfg.CacheDir, a.Logf)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.latestRelease = rel
	a.mu.Unlock()
	return rel, nil
}

func (a *App) setStepState(stepIdx int, status, detail string, pct float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if stepIdx >= 0 && stepIdx < len(a.steps) {
		a.currentStep = stepIdx + 1
		a.steps[stepIdx].Status = status
		a.steps[stepIdx].Detail = detail
		a.steps[stepIdx].Percent = pct
	}
}

// RunFullSetup executes the 5-step installation & configuration walkthrough.
func (a *App) RunFullSetup(ctx context.Context) error {
	a.mu.Lock()
	if a.setupInProgress {
		a.mu.Unlock()
		return fmt.Errorf("setup is already in progress")
	}
	a.setupInProgress = true
	a.lastError = ""
	a.steps = defaultSetupSteps()
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.setupInProgress = false
		a.downloadMBps = 0
		a.downloadPercent = 0
		a.mu.Unlock()
	}()

	failStep := func(idx int, err error) error {
		a.Logf("ERROR in Step %d: %v", idx+1, err)
		a.setStepState(idx, "error", err.Error(), 0)
		a.mu.Lock()
		a.lastError = err.Error()
		a.mu.Unlock()
		return err
	}

	// Step 1: Environment & Prerequisite Check
	a.setStepState(0, "running", "Verifying Keryx directories and resolving Faithlife Proclaim release...", 20)
	if err := a.cfg.EnsureDirs(); err != nil {
		return failStep(0, err)
	}
	rel, err := a.CheckForUpdates(ctx)
	if err != nil {
		return failStep(0, fmt.Errorf("failed to resolve Proclaim installer: %w", err))
	}
	a.setStepState(0, "done", fmt.Sprintf("Resolved Proclaim v%s from download-confirmation page", rel.Version), 100)

	// Step 2: Proton-GE Compatibility Engine
	a.setStepState(1, "running", "Checking for Proton-GE / Wine compatibility runner...", 5)
	runner, err := proton.EnsureProtonGE(ctx, a.cfg, func(dl, total int64, speed, pct float64) {
		a.mu.Lock()
		a.downloadMBps = speed
		a.downloadPercent = pct
		a.mu.Unlock()
		a.setStepState(1, "running", fmt.Sprintf("Downloading Proton-GE... %.1f%% (%.1f MB/s)", pct, speed), pct)
	}, a.Logf)
	if err != nil {
		// Check if any fallback runner (such as System Wine) is available
		runner = proton.GetActiveRunner(a.cfg)
		if runner == nil {
			return failStep(1, err)
		}
		a.Logf("Falling back to runner: %s", runner.Name)
	}
	a.setStepState(1, "done", fmt.Sprintf("Active Runner: %s", runner.Name), 100)

	// Step 3: Windows 10 Prefix, ICU & DXVK
	a.setStepState(2, "running", "Initializing 64-bit Windows 10 prefix, ICU (HASICUDLL), and DXVK...", 35)
	if err := proton.ProvisionPrefix(ctx, a.cfg, runner, a.Logf); err != nil {
		return failStep(2, err)
	}
	a.setStepState(2, "done", "Windows 10 (Build 19045), ICU 73.2, and DXVK configured", 100)

	// Step 4: Download & Install Faithlife Proclaim
	a.setStepState(3, "running", fmt.Sprintf("Downloading Proclaim %s (Proclaim-x64.msi)...", rel.Version), 2)
	msiPath := filepath.Join(a.cfg.CacheDir, fmt.Sprintf("Proclaim-x64-%s.msi", rel.Version))
	err = installer.DownloadFile(ctx, rel.MSIURL, msiPath, func(dl, total int64, speed, pct float64) {
		a.mu.Lock()
		a.downloadMBps = speed
		a.downloadPercent = pct
		a.mu.Unlock()
		a.setStepState(3, "running", fmt.Sprintf("Downloading Proclaim v%s... %.1f%% (%.1f MB/s)", rel.Version, pct, speed), pct*0.75)
	})
	if err != nil {
		return failStep(3, fmt.Errorf("failed to download Proclaim MSI: %w", err))
	}

	a.setStepState(3, "running", fmt.Sprintf("Installing Proclaim v%s into Proton-GE prefix...", rel.Version), 80)
	if err := proton.InstallProclaimMSI(ctx, a.cfg, runner, msiPath, a.Logf); err != nil {
		return failStep(3, fmt.Errorf("failed to install Proclaim MSI: %w", err))
	}
	_ = a.cfg.UpdateSettings(func(s *config.Settings) {
		s.InstalledVersion = rel.Version
		s.LastSetupCompleted = true
	})
	a.setStepState(3, "done", fmt.Sprintf("Faithlife Proclaim v%s installed", rel.Version), 100)

	// Step 5: Desktop Shortcuts & OAuth Sign-In
	a.setStepState(4, "running", "Creating Linux desktop shortcuts and registering proclaim:// OAuth handler...", 50)
	if _, err := desktop.InstallShortcuts(a.cfg, a.iconBytes, a.Logf); err != nil {
		return failStep(4, fmt.Errorf("failed to create desktop shortcuts: %w", err))
	}
	a.setStepState(4, "done", "Faithlife-Proclaim.desktop & proclaim:// OAuth registered", 100)

	a.Logf("Setup complete! Faithlife Proclaim v%s is ready to launch.", rel.Version)
	return nil
}

// Launch starts Faithlife Proclaim in the background (used by the GUI).
func (a *App) Launch(extraArgs ...string) error {
	_, err := proton.LaunchProclaim(a.cfg, extraArgs, a.Logf)
	return err
}

// LaunchAndWait starts Faithlife Proclaim and waits for the process to exit (used by `keryx launch`).
func (a *App) LaunchAndWait(extraArgs ...string) error {
	cmd, err := proton.LaunchProclaim(a.cfg, extraArgs, a.Logf)
	if err != nil {
		return err
	}
	return cmd.Wait()
}
