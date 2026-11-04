package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"keryx/pkg/config"
	"keryx/pkg/desktop"
	"keryx/pkg/proton"
)

//go:embed ui/index.html
var embeddedUIHTML []byte

//go:embed assets/proclaim.png
var embeddedProclaimIcon []byte

func main() {
	cliMode := flag.Bool("cli", false, "Run the interactive setup walkthrough in the terminal instead of opening the desktop GUI")
	autoYes := flag.Bool("yes", false, "Automatically confirm prompts in CLI setup mode")
	uiPort := flag.Int("port", 0, "Local port for the Keryx desktop UI server (0 = auto)")
	flag.Parse()

	cfg, err := config.NewManager()
	if err != nil {
		log.Fatalf("Failed to initialize Keryx configuration: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := NewApp(cfg, embeddedProclaimIcon)

	args := flag.Args()
	if len(args) > 0 {
		cmd := strings.ToLower(args[0])

		// Direct OAuth URI handling if passed as first arg (e.g. `keryx proclaim://...`)
		if strings.HasPrefix(cmd, "proclaim:") || strings.HasPrefix(cmd, "libronixdls:") {
			if err := app.Launch(args[0]); err != nil {
				log.Fatalf("Failed to launch Proclaim with OAuth URI: %v", err)
			}
			return
		}

		switch cmd {
		case "launch", "run", "open":
			var extra []string
			for _, a := range args[1:] {
				if strings.TrimSpace(a) != "" && a != "%u" && a != "%U" {
					extra = append(extra, a)
				}
			}
			// If Proclaim is already installed, launch it directly and seamlessly!
			if proton.FindProclaimExe(cfg) != "" {
				if err := app.Launch(extra...); err != nil {
					log.Fatalf("Failed to launch Faithlife Proclaim: %v", err)
				}
				return
			}
			// Otherwise, fall through to open the Keryx Setup Walkthrough GUI!

		case "setup", "install", "update":
			runTerminalWalkthrough(ctx, app, *autoYes)
			return

		case "status", "doctor":
			runDoctorStatus(ctx, app)
			return

		case "winecfg", "regedit", "kill":
			if err := proton.RunWineTool(cfg, cmd); err != nil {
				log.Fatalf("Failed to run %s: %v", cmd, err)
			}
			return

		case "help":
			printUsage()
			return
		}
	}

	if *cliMode || (runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "") {
		runTerminalWalkthrough(ctx, app, *autoYes)
		return
	}

	// Check latest Proclaim release from download-confirmation page in background
	go func() {
		_, _ = app.CheckForUpdates(ctx)
	}()

	url, err := startDesktopUIServer(ctx, app, *uiPort)
	if err != nil {
		log.Fatalf("Failed to start Keryx UI server: %v", err)
	}

	fmt.Printf("============================================================\n")
	fmt.Printf("  Keryx v%s — Faithlife Proclaim Launcher for Linux\n", config.AppVersion)
	fmt.Printf("  Data Dir   : %s\n", cfg.DataDir)
	fmt.Printf("  Prefix Dir : %s\n", cfg.PrefixDir)
	fmt.Printf("  Desktop UI : %s\n", url)
	fmt.Printf("============================================================\n")

	launchNativeAppWindow(url)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\nShutting down Keryx...")
}

func runTerminalWalkthrough(ctx context.Context, app *App, autoYes bool) {
	fmt.Printf("============================================================\n")
	fmt.Printf("  Keryx v%s — Guided Faithlife Proclaim Setup Wizard\n", config.AppVersion)
	fmt.Printf("============================================================\n\n")

	st := app.GetState()
	fmt.Printf("  Data Directory   : %s\n", st.DataDir)
	fmt.Printf("  Wine/Proton Pfx  : %s\n", st.PrefixDir)
	if st.IsInstalled {
		fmt.Printf("  Current Install  : Faithlife Proclaim (%s)\n", st.InstalledVersion)
	} else {
		fmt.Printf("  Current Install  : Not Installed\n")
	}
	if st.ActiveRunner != nil {
		fmt.Printf("  Detected Runner  : %s (%s)\n", st.ActiveRunner.Name, st.ActiveRunner.Wine64Path)
	} else {
		fmt.Printf("  Detected Runner  : None (Keryx will download latest Proton-GE)\n")
	}

	fmt.Println("\nKeryx will perform the following 5 steps:")
	for _, s := range st.Steps {
		fmt.Printf("  [%d] %s\n      %s\n", s.ID, s.Title, s.Description)
	}

	if !autoYes {
		fmt.Printf("\nProceed with guided installation and setup? [Y/n]: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(strings.ToLower(line))
		if line != "" && line != "y" && line != "yes" {
			fmt.Println("Setup cancelled.")
			return
		}
	}

	fmt.Println()
	if err := app.RunFullSetup(ctx); err != nil {
		fmt.Printf("\n[!] Setup failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n============================================================\n")
	fmt.Printf("  ✓ Faithlife Proclaim is installed and ready!\n")
	fmt.Printf("  • Launch anytime from your app menu ('Faithlife Proclaim')\n")
	fmt.Printf("  • Or run: keryx launch\n")
	fmt.Printf("============================================================\n")
}

func runDoctorStatus(ctx context.Context, app *App) {
	fmt.Printf("Checking latest Faithlife Proclaim release from %s...\n\n", config.ProclaimDownloadConfirmationURL)
	rel, err := app.CheckForUpdates(ctx)
	st := app.GetState()

	fmt.Printf("============================================================\n")
	fmt.Printf("  Keryx v%s — System & Proclaim Status\n", config.AppVersion)
	fmt.Printf("============================================================\n")
	fmt.Printf("  Host OS / Arch      : %s / %s\n", st.HostOS, st.HostArch)
	fmt.Printf("  Proclaim Installed  : %v (%s)\n", st.IsInstalled, st.InstalledVersion)
	if st.ProclaimExePath != "" {
		fmt.Printf("  Proclaim.exe Path   : %s\n", st.ProclaimExePath)
	}
	if err == nil && rel != nil {
		fmt.Printf("  Latest Remote Ver   : %s\n", rel.Version)
		fmt.Printf("  Bootstrapper URL    : %s\n", rel.BootstrapperURL)
		fmt.Printf("  WebScript URL       : %s\n", rel.WebScriptURL)
		fmt.Printf("  Resolved 64-bit MSI : %s\n", rel.MSIURL)
	} else if err != nil {
		fmt.Printf("  Latest Remote Check : Error (%v)\n", err)
	}
	if st.ActiveRunner != nil {
		fmt.Printf("  Active Runner       : %s [%s]\n", st.ActiveRunner.Name, st.ActiveRunner.Type)
		fmt.Printf("  Wine64 Binary       : %s\n", st.ActiveRunner.Wine64Path)
	} else {
		fmt.Printf("  Active Runner       : None installed yet (will auto-download Proton-GE)\n")
	}
	fmt.Printf("  Desktop Shortcuts   : %v (%s)\n", st.DesktopStatus.Installed, st.DesktopStatus.DesktopEntryPath)
	fmt.Printf("============================================================\n")
}

func printUsage() {
	fmt.Printf(`Keryx v%s — Faithlife Proclaim Launcher & Installer for Linux

Usage:
  keryx                   Open the Keryx Desktop Launcher & Setup Walkthrough UI
  keryx launch [uri]      Launch Faithlife Proclaim directly (or forward a proclaim:// OAuth URI)
  keryx setup [--yes]     Run the interactive 5-step Setup Walkthrough in the terminal
  keryx status            Check Proclaim release feed, installed version, and Proton-GE runner status
  keryx winecfg           Open Wine configuration for the Proclaim prefix
  keryx regedit           Open Registry Editor for the Proclaim prefix
  keryx kill              Stop running Proclaim / Wine processes in the prefix
`, config.AppVersion)
}

func startDesktopUIServer(ctx context.Context, app *App, preferredPort int) (string, error) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(embeddedUIHTML)
	})

	mux.HandleFunc("/api/icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(embeddedProclaimIcon)
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, app.GetState())
	})

	mux.HandleFunc("/api/setup/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		go func() {
			_ = app.RunFullSetup(ctx)
		}()
		writeJSON(w, map[string]string{"status": "started"})
	})

	mux.HandleFunc("/api/launch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URI string `json:"uri"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var args []string
		if strings.TrimSpace(req.URI) != "" {
			args = append(args, strings.TrimSpace(req.URI))
		}
		if err := app.Launch(args...); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "launched"})
	})

	mux.HandleFunc("/api/wine-tool", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tool string `json:"tool"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := proton.RunWineTool(app.cfg, req.Tool); err != nil {
			app.Logf("Wine tool (%s) error: %v", req.Tool, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/api/repair", func(w http.ResponseWriter, r *http.Request) {
		go func() {
			runner := proton.GetActiveRunner(app.cfg)
			if runner == nil {
				app.Logf("Cannot repair prefix: no runner installed yet.")
				return
			}
			_ = proton.ProvisionPrefix(ctx, app.cfg, runner, app.Logf)
			_, _ = desktop.InstallShortcuts(app.cfg, app.iconBytes, app.Logf)
			app.Logf("Prefix and desktop shortcut repair completed.")
		}()
		writeJSON(w, map[string]string{"status": "repairing"})
	})

	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			EnableDXVK     bool   `json:"enableDxvk"`
			EnableEsync    bool   `json:"enableEsync"`
			VirtualDesktop string `json:"virtualDesktop"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			_ = app.cfg.UpdateSettings(func(s *config.Settings) {
				s.EnableDXVK = req.EnableDXVK
				s.EnableEsync = req.EnableEsync
				s.EnableFsync = req.EnableEsync
				s.VirtualDesktop = req.VirtualDesktop
			})
			app.Logf("Updated settings (DXVK=%v, ESYNC=%v, VirtualDesktop=%q)", req.EnableDXVK, req.EnableEsync, req.VirtualDesktop)
		}
		writeJSON(w, app.GetState())
	})

	addr := fmt.Sprintf("127.0.0.1:%d", preferredPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		_ = srv.Serve(ln)
	}()

	return fmt.Sprintf("http://%s", ln.Addr().String()), nil
}

func launchNativeAppWindow(url string) {
	switch runtime.GOOS {
	case "linux":
		browsers := []string{
			"chromium",
			"chromium-browser",
			"google-chrome",
			"google-chrome-stable",
			"brave-browser",
			"microsoft-edge",
			"vivaldi",
		}
		for _, b := range browsers {
			if p, err := exec.LookPath(b); err == nil {
				cmd := exec.Command(p, "--app="+url, "--window-size=1020,720")
				if err := cmd.Start(); err == nil {
					return
				}
			}
		}
		_ = exec.Command("xdg-open", url).Start()

	case "windows":
		edgePaths := []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		}
		for _, p := range edgePaths {
			if _, err := os.Stat(p); err == nil {
				_ = exec.Command(p, "--app="+url, "--window-size=1020,720").Start()
				return
			}
		}
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()

	case "darwin":
		chromeApp := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if _, err := os.Stat(chromeApp); err == nil {
			_ = exec.Command(chromeApp, "--app="+url, "--window-size=1020,720").Start()
			return
		}
		_ = exec.Command("open", url).Start()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
