package proton

import (
	"archive/tar"
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

	// 1. Keryx managed Proton-GE directory
	addProtonDir(cfg.RunnersDir, "proton-ge")

	// 2. User's existing Steam Proton-GE / compatibilitytools.d locations
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

// BuildEnv constructs the environment variables for running Wine/Proton-GE seamlessly.
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
	envMap["WINEDEBUG"] = "-all"

	dllOverrides := "winemenubuilder.exe=d;mscoree="
	if settings.EnableDXVK && (runner.Type == "proton-ge" || runner.Type == "steam-proton") {
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
	}

	var result []string
	for k, v := range envMap {
		result = append(result, k+"="+v)
	}
	return result
}

// ProvisionPrefix initializes the 64-bit Wine prefix, configures Windows 10 Build 19045,
// installs Windows ICU globalization files (required by HASICUDLL), and sets up DXVK.
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

	// 1. Apply Windows 10 Build 19045 + Proclaim registry settings
	if logf != nil {
		logf("Applying Windows 10 (Build 19045) & Proclaim registry configuration...")
	}
	if err := applyPrefixRegistry(ctx, cfg, runner, logf); err != nil {
		return fmt.Errorf("failed to configure prefix registry: %w", err)
	}

	// 2. Ensure Windows ICU DLLs & icudtl.dat are installed in system32 (satisfies HASICUDLL)
	if logf != nil {
		logf("Provisioning Windows ICU globalization files (icu.dll / icudtl.dat)...")
	}
	if err := ensureICUFiles(ctx, cfg, runner, logf); err != nil {
		return fmt.Errorf("failed to provision ICU files: %w", err)
	}

	// 3. Configure DXVK for Direct3D 11 hardware acceleration if enabled
	if cfg.GetSettings().EnableDXVK && (runner.Type == "proton-ge" || runner.Type == "steam-proton") {
		if logf != nil {
			logf("Configuring Proton-GE DXVK (Direct3D 11 over Vulkan) for Proclaim...")
		}
		configureDXVK(cfg, runner, logf)
	}

	return nil
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

[HKEY_CURRENT_USER\Software\Proclaim]
"SuppressStartOnInstall"=dword:00000001

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
	sys32 := filepath.Join(cfg.PrefixDir, "drive_c", "windows", "system32")
	if err := os.MkdirAll(sys32, 0755); err != nil {
		return err
	}

	// Copy Proton's built-in x86_64-windows ICU DLLs if available
	if runner != nil && (runner.Type == "proton-ge" || runner.Type == "steam-proton") {
		filesDir := filepath.Dir(filepath.Dir(runner.Wine64Path))
		winSysDir := filepath.Join(filesDir, "lib64", "wine", "x86_64-windows")
		for _, dll := range []string{"icu.dll", "icuin.dll", "icuuc.dll"} {
			src := filepath.Join(winSysDir, dll)
			dst := filepath.Join(sys32, dll)
			if _, err := os.Stat(src); err == nil {
				_ = copyFile(src, dst, 0755)
			}
		}
	}

	// Also ensure icu-win package (icu*.dll + icudtl.dat) is present in system32
	icudtlPath := filepath.Join(sys32, "icudtl.dat")
	icuDllPath := filepath.Join(sys32, "icu.dll")
	if _, err1 := os.Stat(icudtlPath); err1 == nil {
		if _, err2 := os.Stat(icuDllPath); err2 == nil {
			return nil
		}
	}

	icuTarPath := filepath.Join(cfg.CacheDir, "icu-win.tar.gz")
	if err := installer.DownloadFile(ctx, config.ICUWinFallbackURL, icuTarPath, nil); err != nil {
		if logf != nil {
			logf("Warning: could not download icu-win.tar.gz (%v); checking built-in ICU...", err)
		}
		return nil
	}

	icuExtractDir := filepath.Join(cfg.CacheDir, "icu-extracted")
	_ = os.RemoveAll(icuExtractDir)
	if err := extractTarGz(icuTarPath, icuExtractDir); err != nil {
		return err
	}

	// Walk extracted ICU files and copy any .dll or .dat into drive_c/windows/system32
	return filepath.Walk(icuExtractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if ext == ".dll" || ext == ".dat" {
			dst := filepath.Join(sys32, info.Name())
			_ = copyFile(path, dst, 0755)
		}
		return nil
	})
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

	uiFlag := "/qb"
	if cfg.GetSettings().UseMsiQuietMode {
		uiFlag = "/qn"
	}

	if logf != nil {
		logf("Installing %s via Proton-GE msiexec (%s)...", filepath.Base(msiPath), uiFlag)
	}
	err := runWineCmd(ctx, cfg, runner, logf, "msiexec", "/i", msiPath, "REBOOT=ReallySuppress", "INSTALLDESKTOPSHORTCUT=1", uiFlag)
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

	args := append([]string{exePath}, extraArgs...)
	if logf != nil {
		logf("Launching Faithlife Proclaim via %s: %s %v", runner.Name, runner.Wine64Path, args)
	}

	cmd := exec.Command(runner.Wine64Path, args...)
	cmd.Dir = filepath.Dir(exePath)
	cmd.Env = BuildEnv(cfg, runner)

	logFilePath := filepath.Join(cfg.LogsDir, "proclaim-runtime.log")
	if lf, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
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
