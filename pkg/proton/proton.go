package proton

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
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
	"unicode/utf16"

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
	envMap["DOTNET_CLI_UI_LANGUAGE"] = "en-US"

	if runner != nil {
		envMap["WINE"] = runner.Wine64Path
		envMap["WINELOADER"] = runner.Wine64Path
		if runner.WineServer != "" {
			envMap["WINESERVER"] = runner.WineServer
		}
	}

	// Force native Windows ICU DLLs (from icu-win.tar.gz) and disable winemenubuilder.
	// IMPORTANT: Never disable mscoree.dll! Wine's ntdll PE loader (fixup_imports_ilonly) requires
	// builtin mscoree.dll to map IL-only .NET assemblies such as System.Runtime.dll.
	dllOverrides := "icu,icuin,icuuc=n,b;winemenubuilder.exe=d"
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

		wineDllPaths := []string{
			filepath.Join(lib64, "wine", "vkd3d-proton", "x86_64-windows"),
			filepath.Join(lib64, "wine", "x86_64-windows"),
			filepath.Join(lib64, "wine", "x86_64-unix"),
			filepath.Join(lib64, "wine"),
			filepath.Join(lib32, "wine", "vkd3d-proton", "i386-windows"),
			filepath.Join(lib32, "wine", "i386-windows"),
			filepath.Join(lib32, "wine", "i386-unix"),
			filepath.Join(lib32, "wine"),
		}
		envMap["WINEDLLPATH"] = strings.Join(wineDllPaths, string(os.PathListSeparator))

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
// installs Windows ICU globalization files (required by HASICUDLL & .NET 10), installs core fonts, and configures Direct3D.
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

	// 1. Ensure core Windows & WPF fonts exist in drive_c/windows/Fonts with genuine OpenType family names
	ensureCoreFonts(cfg, runner, logf)

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

	// 4. Configure Direct3D DLLs (WineD3D default vs DXVK)
	configureDirect3D(cfg, runner, logf)

	return nil
}

type wpfFontSpec struct {
	FileName  string
	Family    string
	Subfamily string
}

var coreWPFFonts = []wpfFontSpec{
	{"arial.ttf", "Arial", "Regular"},
	{"arialbd.ttf", "Arial", "Bold"},
	{"ariali.ttf", "Arial", "Italic"},
	{"arialbi.ttf", "Arial", "Bold Italic"},
	{"segoeui.ttf", "Segoe UI", "Regular"},
	{"segoeuib.ttf", "Segoe UI", "Bold"},
	{"segoeuii.ttf", "Segoe UI", "Italic"},
	{"segoeuiz.ttf", "Segoe UI", "Bold Italic"},
	{"seguisym.ttf", "Segoe UI Symbol", "Regular"},
	{"tahoma.ttf", "Tahoma", "Regular"},
	{"tahomabd.ttf", "Tahoma", "Bold"},
	{"micross.ttf", "Microsoft Sans Serif", "Regular"},
	{"times.ttf", "Times New Roman", "Regular"},
	{"timesbd.ttf", "Times New Roman", "Bold"},
	{"cour.ttf", "Courier New", "Regular"},
	{"courbd.ttf", "Courier New", "Bold"},
	{"verdana.ttf", "Verdana", "Regular"},
	{"verdanab.ttf", "Verdana", "Bold"},
	{"trebuc.ttf", "Trebuchet MS", "Regular"},
	{"georgia.ttf", "Georgia", "Regular"},
	{"l_10646.ttf", "Lucida Sans Unicode", "Regular"},
	{"lucon.ttf", "Lucida Console", "Regular"},
	{"msyh.ttf", "Microsoft YaHei", "Regular"},
	{"msgothic.ttf", "MS Gothic", "Regular"},
	{"simsun.ttf", "SimSun", "Regular"},
	{"gulim.ttf", "Gulim", "Regular"},
	{"sylfaen.ttf", "Sylfaen", "Regular"},
	{"marlett.ttf", "Marlett", "Regular"},
	{"symbol.ttf", "Symbol", "Regular"},
	{"wingding.ttf", "Wingdings", "Regular"},
	{"webdings.ttf", "Webdings", "Regular"},
}

// ensureCoreFonts populates drive_c/windows/Fonts from Proton/Wine's share/wine/fonts directory
// and guarantees every core Windows/WPF font family has its exact OpenType name table entry
// so WPF's MS.Internal.Shaping.TypefaceMap.MapUnresolvedCharacters never triggers FailFast.
func ensureCoreFonts(cfg *config.Manager, runner *Runner, logf func(string, ...any)) {
	fontsDir := filepath.Join(cfg.PrefixDir, "drive_c", "windows", "Fonts")
	_ = os.MkdirAll(fontsDir, 0755)
	_ = os.Remove(filepath.Join(fontsDir, "Arial.ttf"))

	// 1. Copy all fonts shipped with Proton/Wine (<runner>/files/share/wine/fonts) into drive_c/windows/Fonts
	var wineFontDirs []string
	if runner != nil {
		filesDir := filepath.Dir(filepath.Dir(runner.Wine64Path))
		wineFontDirs = append(wineFontDirs,
			filepath.Join(filesDir, "share", "wine", "fonts"),
			filepath.Join(runner.RootDir, "files", "share", "wine", "fonts"),
			filepath.Join(runner.RootDir, "dist", "share", "wine", "fonts"),
		)
	}
	wineFontDirs = append(wineFontDirs, "/usr/share/wine/fonts", "/usr/local/share/wine/fonts")

	for _, wfd := range wineFontDirs {
		entries, err := os.ReadDir(wfd)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".ttf" || ext == ".ttc" || ext == ".otf" || ext == ".fon" {
				dst := filepath.Join(fontsDir, e.Name())
				if _, err := os.Stat(dst); err != nil {
					_ = copyFile(filepath.Join(wfd, e.Name()), dst, 0644)
				}
			}
		}
	}

	// 2. Find a base TrueType (.ttf) font to synthesize any missing or misnamed WPF fallback fonts
	var baseTTF []byte
	baseCandidates := []string{
		filepath.Join(fontsDir, "tahoma.ttf"),
		filepath.Join(fontsDir, "arial.ttf"),
		"/usr/share/fonts/truetype/msttcorefonts/Arial.ttf",
		"/usr/share/fonts/truetype/msttcorefonts/arial.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/usr/share/fonts/truetype/liberation2/LiberationSans-Regular.ttf",
		"/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
	}
	for _, c := range baseCandidates {
		if data, err := os.ReadFile(c); err == nil && len(data) > 1024 && bytes.Equal(data[0:4], []byte{0, 1, 0, 0}) {
			baseTTF = data
			break
		}
	}
	if len(baseTTF) == 0 {
		_ = filepath.WalkDir("/usr/share/fonts", func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.EqualFold(filepath.Ext(d.Name()), ".ttf") {
				if data, err := os.ReadFile(path); err == nil && len(data) > 1024 && bytes.Equal(data[0:4], []byte{0, 1, 0, 0}) {
					baseTTF = data
					return io.EOF
				}
			}
			return nil
		})
	}
	if len(baseTTF) == 0 {
		return
	}

	synthesizedCount := 0
	for _, spec := range coreWPFFonts {
		dst := filepath.Join(fontsDir, spec.FileName)
		if existing, err := os.ReadFile(dst); err == nil && TTFHasFamilyName(existing, spec.Family) {
			continue
		}
		patched, err := BuildTTFWithFamilyName(baseTTF, spec.Family, spec.Subfamily)
		if err == nil {
			_ = os.WriteFile(dst, patched, 0644)
			synthesizedCount++
		}
	}
	if synthesizedCount > 0 && logf != nil {
		logf("Provisioned %d core Windows/WPF TrueType font families in %s", synthesizedCount, fontsDir)
	}
}

func encodeUTF16BE(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	buf := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.BigEndian.PutUint16(buf[i*2:], v)
	}
	return buf
}

// TTFHasFamilyName inspects the OpenType 'name' table of ttfData and checks if PlatformID=3, NameID=1 matches familyName.
func TTFHasFamilyName(ttfData []byte, familyName string) bool {
	if len(ttfData) < 12 {
		return false
	}
	numTables := int(binary.BigEndian.Uint16(ttfData[4:6]))
	if len(ttfData) < 12+numTables*16 {
		return false
	}
	wantUTF16 := encodeUTF16BE(familyName)
	for i := 0; i < numTables; i++ {
		ent := ttfData[12+i*16 : 12+(i+1)*16]
		if string(ent[0:4]) == "name" {
			off := int(binary.BigEndian.Uint32(ent[8:12]))
			length := int(binary.BigEndian.Uint32(ent[12:16]))
			if off < 0 || length < 6 || off+length > len(ttfData) {
				return false
			}
			nt := ttfData[off : off+length]
			count := int(binary.BigEndian.Uint16(nt[2:4]))
			strOff := int(binary.BigEndian.Uint16(nt[4:6]))
			if 6+count*12 > len(nt) {
				return false
			}
			for j := 0; j < count; j++ {
				rec := nt[6+j*12 : 6+(j+1)*12]
				pid := binary.BigEndian.Uint16(rec[0:2])
				nid := binary.BigEndian.Uint16(rec[6:8])
				rLen := int(binary.BigEndian.Uint16(rec[8:10]))
				rOff := int(binary.BigEndian.Uint16(rec[10:12]))
				if pid == 3 && nid == 1 && strOff+rOff+rLen <= len(nt) {
					if bytes.Equal(nt[strOff+rOff:strOff+rOff+rLen], wantUTF16) {
						return true
					}
				}
			}
		}
	}
	return false
}

// BuildTTFWithFamilyName returns a copy of srcTTF with a spec-compliant OpenType 'name' table
// declaring familyName and subfamilyName for both Macintosh (PlatformID=1) and Windows (PlatformID=3, en-US).
func BuildTTFWithFamilyName(srcTTF []byte, familyName, subfamilyName string) ([]byte, error) {
	if len(srcTTF) < 12 {
		return nil, fmt.Errorf("invalid TTF header")
	}
	numTables := int(binary.BigEndian.Uint16(srcTTF[4:6]))
	if len(srcTTF) < 12+numTables*16 {
		return nil, fmt.Errorf("truncated TTF directory")
	}

	nameEntryIdx := -1
	headOffset := -1
	for i := 0; i < numTables; i++ {
		ent := srcTTF[12+i*16 : 12+(i+1)*16]
		tag := string(ent[0:4])
		if tag == "name" {
			nameEntryIdx = i
		} else if tag == "head" {
			headOffset = int(binary.BigEndian.Uint32(ent[8:12]))
		}
	}
	if nameEntryIdx < 0 {
		return nil, fmt.Errorf("no 'name' table in TTF")
	}

	fullName := familyName
	if subfamilyName != "" && subfamilyName != "Regular" {
		fullName = familyName + " " + subfamilyName
	}
	psName := strings.ReplaceAll(familyName, " ", "") + "-" + strings.ReplaceAll(subfamilyName, " ", "")
	uniqueID := fullName + " : Keryx"
	versionStr := "Version 1.00"

	type namePair struct {
		nameID uint16
		val    string
	}
	pairs := []namePair{
		{1, familyName},
		{2, subfamilyName},
		{3, uniqueID},
		{4, fullName},
		{5, versionStr},
		{6, psName},
	}

	type rawRec struct {
		platformID uint16
		encodingID uint16
		languageID uint16
		nameID     uint16
		data       []byte
	}
	var recs []rawRec
	for _, p := range pairs {
		recs = append(recs, rawRec{1, 0, 0, p.nameID, []byte(p.val)})
	}
	for _, p := range pairs {
		recs = append(recs, rawRec{3, 1, 0x0409, p.nameID, encodeUTF16BE(p.val)})
	}

	strStorageOffset := 6 + len(recs)*12
	var strBuf bytes.Buffer
	var nameTable bytes.Buffer
	_ = binary.Write(&nameTable, binary.BigEndian, uint16(0))
	_ = binary.Write(&nameTable, binary.BigEndian, uint16(len(recs)))
	_ = binary.Write(&nameTable, binary.BigEndian, uint16(strStorageOffset))

	for _, r := range recs {
		_ = binary.Write(&nameTable, binary.BigEndian, r.platformID)
		_ = binary.Write(&nameTable, binary.BigEndian, r.encodingID)
		_ = binary.Write(&nameTable, binary.BigEndian, r.languageID)
		_ = binary.Write(&nameTable, binary.BigEndian, r.nameID)
		_ = binary.Write(&nameTable, binary.BigEndian, uint16(len(r.data)))
		_ = binary.Write(&nameTable, binary.BigEndian, uint16(strBuf.Len()))
		strBuf.Write(r.data)
	}
	nameTable.Write(strBuf.Bytes())
	newNameBytes := nameTable.Bytes()

	out := make([]byte, len(srcTTF))
	copy(out, srcTTF)
	for len(out)%4 != 0 {
		out = append(out, 0)
	}
	newNameOffset := uint32(len(out))
	newNameLen := uint32(len(newNameBytes))
	newNameChecksum := ttfCalcChecksum(newNameBytes)

	out = append(out, newNameBytes...)
	for len(out)%4 != 0 {
		out = append(out, 0)
	}

	entPos := 12 + nameEntryIdx*16
	binary.BigEndian.PutUint32(out[entPos+4:entPos+8], newNameChecksum)
	binary.BigEndian.PutUint32(out[entPos+8:entPos+12], newNameOffset)
	binary.BigEndian.PutUint32(out[entPos+12:entPos+16], newNameLen)

	if headOffset >= 0 && headOffset+12 <= len(out) {
		binary.BigEndian.PutUint32(out[headOffset+8:headOffset+12], 0)
		adj := uint32(0xB1B0AFBA) - ttfCalcChecksum(out)
		binary.BigEndian.PutUint32(out[headOffset+8:headOffset+12], adj)
	}
	return out, nil
}

func ttfCalcChecksum(data []byte) uint32 {
	var sum uint32
	n := len(data) &^ 3
	for i := 0; i < n; i += 4 {
		sum += binary.BigEndian.Uint32(data[i : i+4])
	}
	if rem := len(data) - n; rem > 0 {
		var tail [4]byte
		copy(tail[:], data[n:])
		sum += binary.BigEndian.Uint32(tail[:])
	}
	return sum
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

	// Set renderer=gdi when DXVK is off (prevents WPF wpfgfx_cor3.dll + LogosCEF.exe crash on D3D9 surface sharing)
	d3dBlock := `[HKEY_CURRENT_USER\Software\Wine\Direct3D]
"renderer"="gdi"
`
	if settings.EnableDXVK {
		d3dBlock = `[HKEY_CURRENT_USER\Software\Wine\Direct3D]
"renderer"=-
`
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
"Arial Bold (TrueType)"="arialbd.ttf"
"Arial Italic (TrueType)"="ariali.ttf"
"Arial Bold Italic (TrueType)"="arialbi.ttf"
"Courier New (TrueType)"="cour.ttf"
"Courier New Bold (TrueType)"="courbd.ttf"
"Georgia (TrueType)"="georgia.ttf"
"Gulim (TrueType)"="gulim.ttf"
"Lucida Console (TrueType)"="lucon.ttf"
"Lucida Sans Unicode (TrueType)"="l_10646.ttf"
"Marlett (TrueType)"="marlett.ttf"
"Microsoft Sans Serif (TrueType)"="micross.ttf"
"Microsoft YaHei (TrueType)"="msyh.ttf"
"MS Gothic (TrueType)"="msgothic.ttf"
"Segoe UI (TrueType)"="segoeui.ttf"
"Segoe UI Bold (TrueType)"="segoeuib.ttf"
"Segoe UI Italic (TrueType)"="segoeuii.ttf"
"Segoe UI Bold Italic (TrueType)"="segoeuiz.ttf"
"Segoe UI Symbol (TrueType)"="seguisym.ttf"
"SimSun (TrueType)"="simsun.ttf"
"Sylfaen (TrueType)"="sylfaen.ttf"
"Symbol (TrueType)"="symbol.ttf"
"Tahoma (TrueType)"="tahoma.ttf"
"Tahoma Bold (TrueType)"="tahomabd.ttf"
"Times New Roman (TrueType)"="times.ttf"
"Times New Roman Bold (TrueType)"="timesbd.ttf"
"Trebuchet MS (TrueType)"="trebuc.ttf"
"Verdana (TrueType)"="verdana.ttf"
"Verdana Bold (TrueType)"="verdanab.ttf"
"Webdings (TrueType)"="webdings.ttf"
"Wingdings (TrueType)"="wingding.ttf"

[HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\SecurityProviders\SCHANNEL\Protocols\TLS 1.1\Client]
"Enabled"=dword:00000001
"DisabledByDefault"=dword:00000000

[HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\SecurityProviders\SCHANNEL\Protocols\TLS 1.2\Client]
"Enabled"=dword:00000001
"DisabledByDefault"=dword:00000000

[HKEY_CURRENT_USER\Software\Wine\DllOverrides]
"icu"="native,builtin"
"icuin"="native,builtin"
"icuuc"="native,builtin"
"winemenubuilder.exe"=""
"mscoree"=-

[HKEY_CURRENT_USER\Software\Proclaim]
"SuppressStartOnInstall"=dword:00000000
"LSPExclusions"=dword:00000001

[HKEY_CURRENT_USER\Control Panel\Desktop]
"FontSmoothing"="2"
"FontSmoothingType"=dword:00000002
"FontSmoothingGamma"=dword:00000578
"FontSmoothingOrientation"=dword:00000001

%s
%s`, d3dBlock, desktopBlock)

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

	icuTarPath := filepath.Join(cfg.CacheDir, "icu-win.tar.gz")
	if _, err := os.Stat(icuTarPath); err != nil {
		if logf != nil {
			logf("Downloading FaithLife-Community Windows ICU package (%s)...", config.ICUWinFallbackURL)
		}
		if err := installer.DownloadFile(ctx, config.ICUWinFallbackURL, icuTarPath, nil); err != nil {
			return fmt.Errorf("failed to download icu-win.tar.gz: %w", err)
		}
	}

	// Check if globalization/ICU/icudtl.dat AND genuine native icu.dll (5.3MB in icu-win.tar.gz) are already in place
	if st1, err1 := os.Stat(filepath.Join(globLower, "icudtl.dat")); err1 == nil && st1.Size() > 100000 {
		if st2, err2 := os.Stat(filepath.Join(sys32, "icu.dll")); err2 == nil && st2.Size() > 4000000 {
			return nil
		}
	}

	// Extract icu-win.tar.gz directly into drive_c (replaces Wine stub icu*.dll in system32 & syswow64 and adds globalization/ICU)
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

func configureDirect3D(cfg *config.Manager, runner *Runner, logf func(string, ...any)) {
	if runner == nil || (runner.Type != "proton-ge" && runner.Type != "steam-proton") {
		return
	}
	filesDir := filepath.Dir(filepath.Dir(runner.Wine64Path))
	sys32 := filepath.Join(cfg.PrefixDir, "drive_c", "windows", "system32")

	// Ensure libvkd3d-1.dll and libvkd3d-shader-1.dll are present in system32 so wined3d.dll / d3d9.dll never fail to import them
	vkd3dDirs := []string{
		filepath.Join(filesDir, "lib64", "wine", "vkd3d-proton", "x86_64-windows"),
		filepath.Join(filesDir, "lib64", "wine", "vkd3d-proton"),
		filepath.Join(filesDir, "lib64", "wine", "x86_64-windows"),
		filepath.Join(filesDir, "bin"),
	}
	for _, vdir := range vkd3dDirs {
		for _, vdll := range []string{"libvkd3d-1.dll", "libvkd3d-shader-1.dll"} {
			src := filepath.Join(vdir, vdll)
			if _, err := os.Stat(src); err == nil {
				_ = copyFile(src, filepath.Join(sys32, vdll), 0755)
			}
		}
	}

	if cfg.GetSettings().EnableDXVK {
		candidates := []string{
			filepath.Join(filesDir, "lib64", "wine", "dxvk", "x86_64-windows"),
			filepath.Join(filesDir, "lib64", "wine", "dxvk"),
		}
		for _, dir := range candidates {
			if _, err := os.Stat(filepath.Join(dir, "d3d11.dll")); err == nil {
				for _, dll := range []string{"d3d11.dll", "dxgi.dll", "d3d9.dll", "d3d10core.dll"} {
					src := filepath.Join(dir, dll)
					if _, err := os.Stat(src); err == nil {
						_ = copyFile(src, filepath.Join(sys32, dll), 0755)
					}
				}
				if logf != nil {
					logf("Configured DXVK Direct3D DLLs from %s", dir)
				}
				return
			}
		}
	} else {
		// Restore Proton's standard WineD3D DLLs in case DXVK DLLs were previously copied into system32
		wineWinDir := filepath.Join(filesDir, "lib64", "wine", "x86_64-windows")
		for _, dll := range []string{"d3d11.dll", "dxgi.dll", "d3d9.dll", "d3d10core.dll"} {
			src := filepath.Join(wineWinDir, dll)
			if _, err := os.Stat(src); err == nil {
				_ = copyFile(src, filepath.Join(sys32, dll), 0755)
			}
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

// promotePendingDir promotes AppData\Local\Proclaim\Pending -> AppData\Local\Proclaim\System if Pending exists.
// On Windows, the native Proclaim.SplashScreen wrapper performs this rename and then runs UAC "runas"
// ("/lsp" and "/tls12"). Promoting Pending -> System directly in Keryx lets us launch the real .NET 10
// System\Proclaim.exe binary directly, completely bypassing the UAC elevation prompt in Wine.
func promotePendingDir(proclaimRoot string) {
	pendingDir := filepath.Join(proclaimRoot, "Pending")
	pendingExe := filepath.Join(pendingDir, "Proclaim.exe")
	if _, err := os.Stat(pendingExe); err != nil {
		return
	}
	systemDir := filepath.Join(proclaimRoot, "System")
	_ = os.RemoveAll(systemDir)
	_ = os.Rename(pendingDir, systemDir)
}

// FindProclaimExe searches the Wine prefix for the actual .NET 10 System\Proclaim.exe (promoting Pending -> System if needed).
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
		proclaimRoot := filepath.Join(usersDir, e.Name(), "AppData", "Local", "Proclaim")
		promotePendingDir(proclaimRoot)

		// Prefer launching System\Proclaim.exe directly to bypass Proclaim.SplashScreen's UAC ("runas" /lsp /tls12)
		systemExe := filepath.Join(proclaimRoot, "System", "Proclaim.exe")
		if _, err := os.Stat(systemExe); err == nil {
			return systemExe
		}

		stubExe := filepath.Join(proclaimRoot, "Proclaim.exe")
		if _, err := os.Stat(stubExe); err == nil {
			return stubExe
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
	exeDir := filepath.Dir(exePath)
	candidates := []string{
		filepath.Join(exeDir, "Proclaim.deps.json"),
		filepath.Join(exeDir, "System", "Proclaim.deps.json"),
		filepath.Join(exeDir, "Pending", "Proclaim.deps.json"),
	}
	for _, depsPath := range candidates {
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

// ensureSynchronousNLog rewrites System\NLog.config to remove AsyncWrapper and enable autoFlush="true"
// plus console output, so any startup error or stack trace is immediately flushed to Proclaim.log and stdout.
func ensureSynchronousNLog(systemDir string) {
	nlogPath := filepath.Join(systemDir, "NLog.config")
	if _, err := os.Stat(nlogPath); err != nil {
		return
	}
	syncConfig := `<?xml version="1.0" encoding="utf-8" ?>
<nlog xmlns="http://www.nlog-project.org/schemas/NLog.xsd"
      xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <targets>
    <target name="file"
            xsi:type="File"
            fileName="${specialfolder:folder=LocalApplicationData:dir=Faithlife\Logs\Proclaim}\Proclaim.log"
            layout="${longdate} ${pad:padding=5:inner=${threadid}} ${pad:padding=-5:inner=${level:uppercase=true}} ${logger:shortName=true} ${message}${onexception:${newline}${exception:format=ToString,Data}}"
            keepFileOpen="true"
            openFileCacheTimeout="30"
            autoFlush="true"
            ConcurrentWrites="false"
            deleteOldFileOnStartup="true"
            encoding="utf-8" />
    <target name="console"
            xsi:type="Console"
            layout="${longdate} ${pad:padding=5:inner=${threadid}} ${pad:padding=-5:inner=${level:uppercase=true}} ${logger:shortName=true} ${message}${onexception:${newline}${exception:format=ToString,Data}}" />
    <target name="debugger"
            xsi:type="Debugger"
            layout="${longdate}|${pad:padding=5:inner=${threadid}}|${pad:padding=-5:inner=${level:uppercase=true}}|${logger:shortName=true}|${message}${onexception:${newline}${exception:format=ToString,Data}}" />
  </targets>
  <rules>
    <logger name="*" minlevel="Info" writeTo="file,console,debugger" />
  </rules>
</nlog>
`
	_ = os.WriteFile(nlogPath, []byte(syncConfig), 0644)
}

// IsProclaimRunning checks if Proclaim.exe is currently running.
func IsProclaimRunning() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	out, err := exec.Command("pgrep", "-f", "Proclaim.exe").Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// LaunchProclaim launches System\Proclaim.exe inside the configured Proton-GE prefix.
func LaunchProclaim(cfg *config.Manager, extraArgs []string, logf func(string, ...any)) (*exec.Cmd, error) {
	runner := GetActiveRunner(cfg)
	if runner == nil {
		return nil, fmt.Errorf("no Proton-GE or Wine runner found; please run Setup first")
	}
	exePath := FindProclaimExe(cfg)
	if exePath == "" {
		return nil, fmt.Errorf("Proclaim.exe not found in prefix %s; please install Proclaim first", cfg.PrefixDir)
	}

	// Self-heal ICU data files, core fonts, Direct3D DLLs, registry, and synchronous NLog before launching
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ensureCoreFonts(cfg, runner, logf)
	configureDirect3D(cfg, runner, logf)
	_ = applyPrefixRegistry(ctx, cfg, runner, nil)
	if err := ensureICUFiles(ctx, cfg, runner, logf); err != nil && logf != nil {
		logf("Warning: ICU verification before launch returned: %v", err)
	}
	ensureSynchronousNLog(filepath.Dir(exePath))

	args := append([]string{exePath}, extraArgs...)
	if logf != nil {
		logf("Launching Faithlife Proclaim via %s: %s %v", runner.Name, runner.Wine64Path, args)
	}

	cmd := exec.Command(runner.Wine64Path, args...)
	cmd.Dir = filepath.Dir(exePath)
	cmd.Env = BuildEnv(cfg, runner)

	logFilePath := filepath.Join(cfg.LogsDir, "proclaim-runtime.log")
	lf, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
	}

	if err := cmd.Start(); err != nil {
		if lf != nil {
			_ = lf.Close()
		}
		return nil, err
	}
	if lf != nil {
		// Parent can close its duplicate fd immediately; child and grandchild processes keep their open fd to disk
		_ = lf.Close()
	}

	return cmd, nil
}

// WaitForProclaim waits for both Proclaim.exe and the wineserver session,
// streaming proclaim-runtime.log and dumping any Proclaim.log / ProclaimError.log / ProclaimCrash.txt if it exits.
func WaitForProclaim(cfg *config.Manager, cmd *exec.Cmd, logf func(string, ...any)) error {
	logFilePath := filepath.Join(cfg.LogsDir, "proclaim-runtime.log")
	stopTail := make(chan struct{})

	go func() {
		var offset int64
		for {
			select {
			case <-stopTail:
				return
			case <-time.After(350 * time.Millisecond):
				if f, err := os.Open(logFilePath); err == nil {
					if st, err := f.Stat(); err == nil && st.Size() > offset {
						_, _ = f.Seek(offset, io.SeekStart)
						buf, _ := io.ReadAll(f)
						offset += int64(len(buf))
						for _, line := range strings.Split(string(buf), "\n") {
							if trimmed := strings.TrimSpace(line); trimmed != "" && logf != nil {
								logf("[Wine] %s", trimmed)
							}
						}
					}
					_ = f.Close()
				}
			}
		}
	}()

	_ = cmd.Wait()

	runner := GetActiveRunner(cfg)
	if runner != nil && runner.WineServer != "" {
		wsCmd := exec.Command(runner.WineServer, "-w")
		wsCmd.Env = BuildEnv(cfg, runner)
		_ = wsCmd.Run()
	}

	time.Sleep(400 * time.Millisecond)
	close(stopTail)

	DumpProclaimLogs(cfg, logf)
	return nil
}

// DumpProclaimLogs searches the prefix for Proclaim's own error/crash logs and prints them to logf.
func DumpProclaimLogs(cfg *config.Manager, logf func(string, ...any)) {
	if logf == nil {
		return
	}
	usersDir := filepath.Join(cfg.PrefixDir, "drive_c", "users")
	entries, err := os.ReadDir(usersDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "Public" {
			continue
		}
		logDirs := []string{
			filepath.Join(usersDir, e.Name(), "AppData", "Local", "Faithlife", "Logs", "Proclaim"),
			filepath.Join(usersDir, e.Name(), "Documents", "Proclaim Log Files"),
			filepath.Join(usersDir, e.Name(), "Documents", "Logos Log Files"),
		}
		for _, dir := range logDirs {
			files, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				fullPath := filepath.Join(dir, f.Name())
				data, err := os.ReadFile(fullPath)
				if err != nil || len(data) == 0 {
					continue
				}
				lines := strings.Split(strings.TrimSpace(string(data)), "\n")
				start := 0
				if len(lines) > 45 {
					start = len(lines) - 45
				}
				logf("--- %s (last %d lines) ---", f.Name(), len(lines)-start)
				for _, l := range lines[start:] {
					logf("  %s", strings.TrimRight(l, "\r"))
				}
			}
		}
	}
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
			// Remove existing file/symlink first so we never follow a symlink into read-only runner dirs
			_ = os.Remove(target)
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0644)
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
	_ = os.Remove(dst)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
