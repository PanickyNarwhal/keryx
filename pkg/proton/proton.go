package proton

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"keryx/pkg/config"
	"keryx/pkg/installer"
)

type Runner struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // "proton-ge", "steam-proton", "system-wine"
	RootDir    string `json:"rootDir"`
	Wine64Path string `json:"wine64Path"`
	WineServer string `json:"wineServer"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

var reDepsVersion = regexp.MustCompile(`"Proclaim/([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)"`)

// DiscoverRunners scans Keryx's managed runners directory, Steam compatibilitytools.d, and system PATH.
func DiscoverRunners(cfg *config.Manager) []Runner {
	var runners []Runner
	seen := make(map[string]bool)

	addProtonDir := func(baseDir, runnerType string) {
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			root := filepath.Join(baseDir, e.Name())
			wine64 := filepath.Join(root, "files", "bin", "wine64")
			if _, err := os.Stat(wine64); err != nil {
				wine64 = filepath.Join(root, "dist", "bin", "wine64")
				if _, err2 := os.Stat(wine64); err2 != nil {
					continue
				}
			}
			if seen[wine64] {
				continue
			}
			seen[wine64] = true
			runners = append(runners, Runner{
				Name:       e.Name(),
				Type:       runnerType,
				RootDir:    root,
				Wine64Path: wine64,
				WineServer: filepath.Join(filepath.Dir(wine64), "wineserver"),
			})
		}
	}

	// 1. Keryx managed Proton-GE directory (no Steam required)
	addProtonDir(cfg.RunnersDir, "proton-ge")

	// 2. User's existing Steam Proton-GE / compatibilitytools.d locations (if any)
	if home, err := os.UserHomeDir(); err == nil {
		steamCompatDirs := []string{
			filepath.Join(home, ".steam", "root", "compatibilitytools.d"),
			filepath.Join(home, ".local", "share", "Steam", "compatibilitytools.d"),
			filepath.Join(home, ".var", "app", "com.valvesoftware.Steam", ".local", "share", "Steam", "compatibilitytools.d"),
		}
		for _, d := range steamCompatDirs {
			addProtonDir(d, "steam-proton")
		}
	}

	// Sort Proton runners newest-first
	sort.Slice(runners, func(i, j int) bool {
		return runners[i].Name > runners[j].Name
	})

	// 3. System Wine fallback
	for _, binName := range []string{"wine64", "wine"} {
		if p, err := exec.LookPath(binName); err == nil && !seen[p] {
			seen[p] = true
			ws, _ := exec.LookPath("wineserver")
			runners = append(runners, Runner{
				Name:       "System Wine (" + binName + ")",
				Type:       "system-wine",
				RootDir:    filepath.Dir(filepath.Dir(p)),
				Wine64Path: p,
				WineServer: ws,
			})
			break
		}
	}

	return runners
}

// GetActiveRunner returns the currently configured runner or the best discovered runner.
func GetActiveRunner(cfg *config.Manager) *Runner {
	runners := DiscoverRunners(cfg)
	settings := cfg.GetSettings()

	if settings.SelectedWine64Path != "" {
		for _, r := range runners {
			if r.Wine64Path == settings.SelectedWine64Path {
				rCopy := r
				return &rCopy
			}
		}
	}
	if len(runners) > 0 {
		rCopy := runners[0]
		return &rCopy
	}
	return nil
}

// FetchLatestProtonGERelease queries GitHub API for the latest GloriousEggroll Proton-GE release.
func FetchLatestProtonGERelease(ctx context.Context) (tag, downloadURL string) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, config.ProtonGELatestAPIURL, nil)
	if err != nil {
		return config.ProtonGEFallbackTag, config.ProtonGEFallbackURL
	}
	req.Header.Set("User-Agent", "Keryx/"+config.AppVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return config.ProtonGEFallbackTag, config.ProtonGEFallbackURL
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var rel githubRelease
		if err := json.NewDecoder(resp.Body).Decode(&rel); err == nil && rel.TagName != "" {
			for _, a := range rel.Assets {
				if strings.HasSuffix(a.Name, ".tar.gz") {
					return rel.TagName, a.BrowserDownloadURL
				}
			}
		}
	}
	return config.ProtonGEFallbackTag, config.ProtonGEFallbackURL
}

// EnsureProtonGE downloads and extracts Proton-GE if no Proton runner is available.
func EnsureProtonGE(ctx context.Context, cfg *config.Manager, onProgress installer.ProgressFunc, logf func(string, ...any)) (*Runner, error) {
	if existing := GetActiveRunner(cfg); existing != nil && (existing.Type == "proton-ge" || existing.Type == "steam-proton") {
		if logf != nil {
			logf("Using existing Proton runner: %s (%s)", existing.Name, existing.Wine64Path)
		}
		return existing, nil
	}

	tag, dlURL := FetchLatestProtonGERelease(ctx)
	tarballPath := filepath.Join(cfg.CacheDir, tag+".tar.gz")
	if logf != nil {
		logf("Downloading Proton-GE (%s) from %s...", tag, dlURL)
	}
	if err := installer.DownloadFile(ctx, dlURL, tarballPath, onProgress); err != nil {
		return nil, fmt.Errorf("failed to download Proton-GE: %w", err)
	}

	if logf != nil {
		logf("Extracting %s to %s...", filepath.Base(tarballPath), cfg.RunnersDir)
	}
	if err := extractTarGz(tarballPath, cfg.RunnersDir); err != nil {
		return nil, fmt.Errorf("failed to extract Proton-GE: %w", err)
	}

	runners := DiscoverRunners(cfg)
	for _, r := range runners {
		if r.Type == "proton-ge" {
			_ = cfg.UpdateSettings(func(s *config.Settings) {
				s.RunnerMode = r.Type
				s.SelectedRunnerName = r.Name
				s.SelectedWine64Path = r.Wine64Path
			})
			return &r, nil
		}
	}
	return nil, fmt.Errorf("Proton-GE extracted, but wine64 binary was not found in %s", cfg.RunnersDir)
}

// BuildEnv constructs the environment variables for running Wine/Proton-GE standalone (no Steam required).
func BuildEnv(cfg *config.Manager, runner *Runner) []string {
	settings := cfg.GetSettings()
	envMap := make(map[string]string)
	for _, kv := range os.Environ() {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			envMap[kv[:idx]] = kv[idx+1:]
		}
	}

	envMap["WINEPREFIX"] = cfg.PrefixDir
	envMap["WINEARCH"] = "win64"
	envMap["WINEDEBUG"] = "fixme-all,err+all"

	if runner != nil {
		envMap["WINE"] = runner.Wine64Path
		envMap["WINELOADER"] = runner.Wine64Path
		if runner.WineServer != "" {
			envMap["WINESERVER"] = runner.WineServer
		}
	}

	// Force native Windows ICU DLLs (from FaithLife-Community/icu) and disable Wine-Mono / winemenubuilder
	dllOverrides := "icu,icuin,icuuc=n,b;winemenubuilder.exe=d;mscoree="
	if settings.EnableDXVK && runner != nil && (runner.Type == "proton-ge" || runner.Type == "steam-proton") {
		dllOverrides = "d3d11,dxgi,d3d9,d3d10core=n,b;" + dllOverrides
	}
	envMap["WINEDLLOVERRIDES"] = dllOverrides

	if settings.EnableEsync {
		envMap["WINEESYNC"] = "1"
	}
	if settings.EnableFsync {
		envMap["WINEFSYNC"] = "1"
	}
	envMap["DXVK_LOG_LEVEL"] = "none"

	if runner != nil && (runner.Type == "proton-ge" || runner.Type == "steam-proton") {
		filesDir := filepath.Dir(filepath.Dir(runner.Wine64Path)) // <root>/files
		lib64 := filepath.Join(filesDir, "lib64")
		lib32 := filepath.Join(filesDir, "lib")
		binDir := filepath.Join(filesDir, "bin")

		envMap["PATH"] = binDir + string(os.PathListSeparator) + envMap["PATH"]

		ldPaths := []string{lib64, lib32}
		if existingLD := envMap["LD_LIBRARY_PATH"]; existingLD != "" {
			ldPaths = append(ldPaths, existingLD)
		}
		envMap["LD_LIBRARY_PATH"] = strings.Join(ldPaths, string(os.PathListSeparator))

		gstPaths := []string{
			filepath.Join(lib64, "gstreamer-1.0"),
			filepath.Join(lib32, "gstreamer-1.0"),
		}
		envMap["GST_PLUGIN_SYSTEM_PATH_1_0"] = strings.Join(gstPaths, string(os.PathListSeparator))

		// Standalone Proton compatibility variables so Steam is never required
		steamDummyDir := filepath.Join(cfg.DataDir, "steam_client")
		_ = os.MkdirAll(steamDummyDir, 0755)
		envMap["STEAM_COMPAT_CLIENT_INSTALL_PATH"] = steamDummyDir
		envMap["STEAM_COMPAT_DATA_PATH"] = filepath.Dir(cfg.PrefixDir)
	}

	var result []string
	for k, v := range envMap {
		result = append(result, k+"="+v)
	}
	return result
}

// ProvisionPrefix initializes the 64-bit Wine prefix, configures Windows 10 Build 19045,
// installs Windows ICU globalization files (required by HASICUDLL & .NET 10), installs core fonts, and sets up DXVK.
func ProvisionPrefix(ctx context.Context, cfg *config.Manager, runner *Runner, logf func(string, ...any)) error {
	if runtime.GOOS == "windows" {
		if logf != nil {
			logf("Host OS is Windows; skipping Linux Wine prefix boot.")
		}
		return nil
	}

	if err := os.MkdirAll(cfg.PrefixDir, 0755); err != nil {
		return err
	}

	if logf != nil {
		logf("Initializing 64-bit prefix at %s...", cfg.PrefixDir)
	}
	if err := runWineCmd(ctx, cfg, runner, logf, "wineboot", "-u"); err != nil {
		if logf != nil {
			logf("Note: wineboot returned %v (continuing prefix configuration)", err)
		}
	}

	// 1. Ensure core fonts (Arial) exist in drive_c/windows/Fonts so WPF (.NET 10) doesn't crash on startup
	ensureCoreFonts(cfg, logf)

	// 2. Apply Windows 10 Build 19045 + Proclaim + Font registry settings
	if logf != nil {
		logf("Applying Windows 10 (Build 19045) & Proclaim registry configuration...")
	}
	if err := applyPrefixRegistry(ctx, cfg, runner, logf); err != nil {
		return fmt.Errorf("failed to configure prefix registry: %w", err)
	}

	// 3. Ensure Windows ICU DLLs & globalization/ICU/icudtl.dat are installed
	if logf != nil {
		logf("Provisioning Windows ICU globalization files (icu.dll & globalization/ICU/icudtl.dat)...")
	}
	if err := ensureICUFiles(ctx, cfg, runner, logf); err != nil {
		return fmt.Errorf("failed to provision ICU files: %w", err)
	}

	// 4. Configure DXVK for Direct3D 11 hardware acceleration if enabled
	if cfg.GetSettings().EnableDXVK && (runner.Type == "proton-ge" || runner.Type == "steam-proton") {
		if logf != nil {
			logf("Configuring Proton-GE DXVK (Direct3D 11 over Vulkan) for Proclaim...")
		}
		configureDXVK(cfg, runner, logf)
	}

	return nil
}

// ensureCoreFonts ensures arial.ttf is present in drive_c/windows/Fonts so .NET WPF text rendering works out of the box.
func ensureCoreFonts(cfg *config.Manager, logf func(string, ...any)) {
	fontsDir := filepath.Join(cfg.PrefixDir, "drive_c", "windows", "Fonts")
	_ = os.MkdirAll(fontsDir, 0755)

	arialDest := filepath.Join(fontsDir, "arial.ttf")
	if _, err := os.Stat(arialDest); err == nil {
		return
	}

	// Search common Kubuntu/Ubuntu/Linux system fonts for Arial or a metric-compatible Sans-Serif TrueType font
	candidates := []string{
		"/usr/share/fonts/truetype/msttcorefonts/Arial.ttf",
		"/usr/share/fonts/truetype/msttcorefonts/arial.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/usr/share/fonts/truetype/liberation2/LiberationSans-Regular.ttf",
		"/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
	}

	for _, src := range candidates {
		if _, err := os.Stat(src); err == nil {
			_ = copyFile(src, arialDest, 0644)
			_ = copyFile(src, filepath.Join(fontsDir, "Arial.ttf"), 0644)
			if logf != nil {
				logf("Installed fallback Arial TrueType font from %s", src)
			}
			return
		}
	}
}

func applyPrefixRegistry(ctx context.Context, cfg *config.Manager, runner *Runner, logf func(string, ...any)) error {
	settings := cfg.GetSettings()
	desktopBlock := `[HKEY_CURRENT_USER\Software\Wine\Explorer]
"Desktop"=-
`
	if settings.VirtualDesktop != "" {
		desktopBlock = fmt.Sprintf(`[HKEY_CURRENT_USER\Software\Wine\Explorer]
"Desktop"="Default"

[HKEY_CURRENT_USER\Software\Wine\Explorer\Desktops]
"Default"="%s"
`, settings.VirtualDesktop)
	}

	regContent := fmt.Sprintf(`Windows Registry Editor Version 5.00

[HKEY_LOCAL_MACHINE\Software\Microsoft\Windows NT\CurrentVersion]
"CurrentBuild"="19045"
"CurrentBuildNumber"="19045"
"CurrentMajorVersionNumber"=dword:0000000a
"CurrentMinorVersionNumber"=dword:00000000
"CurrentVersion"="6.3"
"ProductName"="Windows 10 Pro"
"EditionID"="Professional"
"CSDVersion"=""

[HKEY_LOCAL_MACHINE\System\CurrentControlSet\Control\ProductOptions]
"ProductType"="WinNT"

[HKEY_LOCAL_MACHINE\Software\Microsoft\Windows NT\CurrentVersion\Fonts]
"Arial (TrueType)"="arial.ttf"

[HKEY_CURRENT_USER\Software\Wine\DllOverrides]
"winemenubuilder.exe"=""
"icu"="native,builtin"
"icuin"="native,builtin"
"icuuc"="native,builtin"

[HKEY_CURRENT_USER\Software\Proclaim]
"SuppressStartOnInstall"=dword:00000000

[HKEY_CURRENT_USER\Control Panel\Desktop]
"FontSmoothing"="2"
"FontSmoothingType"=dword:00000002
"FontSmoothingGamma"=dword:00000578
"FontSmoothingOrientation"=dword:00000001

%s`, desktopBlock)

	regPath := filepath.Join(cfg.CacheDir, "keryx-win10.reg")
	if err := os.WriteFile(regPath, []byte(regContent), 0644); err != nil {
		return err
	}
	return runWineCmd(ctx, cfg, runner, logf, "regedit", "/S", regPath)
}

func ensureICUFiles(ctx context.Context, cfg *config.Manager, runner *Runner, logf func(string, ...any)) error {
	driveC := filepath.Join(cfg.PrefixDir, "drive_c")
	winDir := filepath.Join(driveC, "windows")
	sys32 := filepath.Join(winDir, "system32")
	globLower := filepath.Join(winDir, "globalization", "ICU")
	globUpper := filepath.Join(winDir, "Globalization", "ICU")

	// Check if both system32/icu.dll and globalization/ICU/icudtl.dat are already installed
	if st1, err1 := os.Stat(filepath.Join(globLower, "icudtl.dat")); err1 == nil && st1.Size() > 100000 {
		if st2, err2 := os.Stat(filepath.Join(sys32, "icu.dll")); err2 == nil && st2.Size() > 50000 {
			return nil
		}
	}

	icuTarPath := filepath.Join(cfg.CacheDir, "icu-win.tar.gz")
	if logf != nil {
		logf("Downloading FaithLife-Community Windows ICU package (%s)...", config.ICUWinFallbackURL)
	}
	if err := installer.DownloadFile(ctx, config.ICUWinFallbackURL, icuTarPath, nil); err != nil {
		return fmt.Errorf("failed to download icu-win.tar.gz: %w", err)
	}

	// Extract icu-win.tar.gz directly into drive_c (populates drive_c/windows/system32 and drive_c/windows/globalization/ICU)
	if err := extractTarGz(icuTarPath, driveC); err != nil {
		return fmt.Errorf("failed to extract icu-win.tar.gz: %w", err)
	}

	// Mirror globalization/ICU to Globalization/ICU and system32 for case-sensitive Linux filesystems
	_ = os.MkdirAll(globUpper, 0755)
	if entries, err := os.ReadDir(globLower); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				src := filepath.Join(globLower, e.Name())
				_ = copyFile(src, filepath.Join(globUpper, e.Name()), 0644)
				if e.Name() == "icudtl.dat" {
					_ = copyFile(src, filepath.Join(sys32, "icudtl.dat"), 0644)
				}
			}
		}
	}

	if logf != nil {
		logf("Installed Windows ICU DLLs and globalization/ICU/icudtl.dat into prefix.")
	}
	return nil
}

func configureDXVK(cfg *config.Manager, runner *Runner, logf func(string, ...any)) {
	filesDir := filepath.Dir(filepath.Dir(runner.Wine64Path))
	candidates := []string{
		filepath.Join(filesDir, "lib64", "wine", "dxvk", "x86_64-windows"),
		filepath.Join(filesDir, "lib64", "wine", "dxvk"),
	}
	sys32 := filepath.Join(cfg.PrefixDir, "drive_c", "windows", "system32")
	for _, dir := range candidates {
		d3d11 := filepath.Join(dir, "d3d11.dll")
		if _, err := os.Stat(d3d11); err == nil {
			for _, dll := range []string{"d3d11.dll", "dxgi.dll", "d3d9.dll", "d3d10core.dll"} {
				src := filepath.Join(dir, dll)
				if _, err := os.Stat(src); err == nil {
					_ = copyFile(src, filepath.Join(sys32, dll), 0755)
				}
			}
			if logf != nil {
				logf("Installed DXVK Direct3D 11 DLLs from %s", dir)
			}
			return
		}
	}
}

// InstallProclaimMSI runs msiexec inside the Proton-GE prefix to install or update Proclaim-x64.msi.
func InstallProclaimMSI(ctx context.Context, cfg *config.Manager, runner *Runner, msiPath string, logf func(string, ...any)) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("running on Windows host; Linux Proton/Wine MSI installation requires Linux")
	}

	uiFlag := "/passive"
	if cfg.GetSettings().UseMsiQuietMode {
		uiFlag = "/qn"
	}

	if logf != nil {
		logf("Installing %s via Proton-GE msiexec (%s)...", filepath.Base(msiPath), uiFlag)
	}
	err := runWineCmd(ctx, cfg, runner, logf, "msiexec", "/i", msiPath, "REBOOT=ReallySuppress", uiFlag)
	if err != nil {
		// Check if Proclaim.exe was installed despite non-zero exit code (e.g. 3010 reboot required)
		if exePath := FindProclaimExe(cfg); exePath != "" {
			if logf != nil {
				logf("msiexec exited with %v, but Proclaim.exe is present at %s", err, exePath)
			}
			return nil
		}
		return err
	}
	return nil
}

// FindProclaimExe searches the Wine prefix for Proclaim.exe.
func FindProclaimExe(cfg *config.Manager) string {
	usersDir := filepath.Join(cfg.PrefixDir, "drive_c", "users")
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "Public" {
			continue
		}
		candidate := filepath.Join(usersDir, e.Name(), "AppData", "Local", "Proclaim", "Proclaim.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// GetInstalledVersion reads Proclaim.deps.json from System/ or Pending/ to determine the installed version.
func GetInstalledVersion(cfg *config.Manager) string {
	exePath := FindProclaimExe(cfg)
	if exePath == "" {
		return ""
	}
	proclaimDir := filepath.Dir(exePath)
	for _, sub := range []string{"System", "Pending"} {
		depsPath := filepath.Join(proclaimDir, sub, "Proclaim.deps.json")
		if data, err := os.ReadFile(depsPath); err == nil {
			if m := reDepsVersion.FindSubmatch(data); len(m) >= 2 {
				return string(m[1])
			}
		}
	}
	if v := cfg.GetSettings().InstalledVersion; v != "" {
		return v
	}
	return "Installed"
}

// IsProclaimRunning checks if Proclaim.exe is currently running.
func IsProclaimRunning() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	out, err := exec.Command("pgrep", "-f", "Proclaim.exe").Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// LaunchProclaim launches Proclaim.exe inside the configured Proton-GE prefix, optionally passing an OAuth URI or args.
func LaunchProclaim(cfg *config.Manager, extraArgs []string, logf func(string, ...any)) (*exec.Cmd, error) {
	runner := GetActiveRunner(cfg)
	if runner == nil {
		return nil, fmt.Errorf("no Proton-GE or Wine runner found; please run Setup first")
	}
	exePath := FindProclaimExe(cfg)
	if exePath == "" {
		return nil, fmt.Errorf("Proclaim.exe not found in prefix %s; please install Proclaim first", cfg.PrefixDir)
	}

	// Self-heal ICU data files & core fonts before launching if missing
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ensureCoreFonts(cfg, logf)
	if err := ensureICUFiles(ctx, cfg, runner, logf); err != nil && logf != nil {
		logf("Warning: ICU verification before launch returned: %v", err)
	}

	args := append([]string{exePath}, extraArgs...)
	if logf != nil {
		logf("Launching Faithlife Proclaim via %s: %s %v", runner.Name, runner.Wine64Path, args)
	}

	cmd := exec.Command(runner.Wine64Path, args...)
	cmd.Dir = filepath.Dir(exePath)
	cmd.Env = BuildEnv(cfg, runner)

	logFilePath := filepath.Join(cfg.LogsDir, "proclaim-runtime.log")
	lf, lfErr := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		if lf != nil {
			_ = lf.Close()
		}
		return nil, err
	}

	// Close write end when process exits and stream runtime output to logf + proclaim-runtime.log
	go func() {
		_ = cmd.Wait()
		_ = pw.Close()
	}()
	go func() {
		defer func() {
			if lfErr == nil && lf != nil {
				_ = lf.Close()
			}
		}()
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			line := scanner.Text()
			if lfErr == nil && lf != nil {
				_, _ = fmt.Fprintln(lf, line)
			}
			if logf != nil && strings.TrimSpace(line) != "" {
				logf("[Proclaim] %s", line)
			}
		}
	}()

	return cmd, nil
}

// RunWineTool launches a Wine utility such as winecfg, regedit, or wineserver -k.
func RunWineTool(cfg *config.Manager, tool string) error {
	runner := GetActiveRunner(cfg)
	if runner == nil {
		return fmt.Errorf("no Proton-GE or Wine runner available")
	}
	if tool == "kill" {
		if runner.WineServer != "" {
			cmd := exec.Command(runner.WineServer, "-k")
			cmd.Env = BuildEnv(cfg, runner)
			return cmd.Run()
		}
		return fmt.Errorf("wineserver binary not found")
	}
	cmd := exec.Command(runner.Wine64Path, tool)
	cmd.Env = BuildEnv(cfg, runner)
	return cmd.Start()
}

func runWineCmd(ctx context.Context, cfg *config.Manager, runner *Runner, logf func(string, ...any), args ...string) error {
	cmd := exec.CommandContext(ctx, runner.Wine64Path, args...)
	cmd.Env = BuildEnv(cfg, runner)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 && logf != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			logf("[wine64 %s] %s", args[0], trimmed)
		}
	}
	return err
}

func extractTarGz(tarGzPath, destDir string) error {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	cleanDest := filepath.Clean(destDir)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(cleanDest, hdr.Name)
		if !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) && target != cleanDest {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				_ = outFile.Close()
				return err
			}
			_ = outFile.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			_ = os.Remove(target)
			_ = os.Symlink(hdr.Linkname, target)
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			linkTarget := filepath.Join(cleanDest, hdr.Linkname)
			_ = os.Remove(target)
			_ = os.Link(linkTarget, target)
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
