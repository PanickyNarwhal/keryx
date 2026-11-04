package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"keryx/pkg/config"
	"keryx/pkg/installer"
	"keryx/pkg/proton"
)

func TestResolveFromDownloadConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	rel, err := installer.ResolveFromDownloadConfirmation(ctx, tmpDir, t.Logf)
	if err != nil {
		t.Fatalf("ResolveFromDownloadConfirmation failed: %v", err)
	}
	if !strings.HasSuffix(rel.BootstrapperURL, "ProclaimSetup.exe") {
		t.Errorf("expected BootstrapperURL to end with ProclaimSetup.exe, got %s", rel.BootstrapperURL)
	}
	if rel.Version == "" {
		t.Errorf("expected non-empty resolved Proclaim version")
	}
	if !strings.HasSuffix(rel.MSIURL, "Proclaim-x64.msi") {
		t.Errorf("expected MSIURL to end with Proclaim-x64.msi, got %s", rel.MSIURL)
	}
	t.Logf("Resolved Proclaim v%s -> %s", rel.Version, rel.MSIURL)
}

func TestBuildEnvIncludesProtonAndDXVK(t *testing.T) {
	t.Setenv("KERYX_HOME", t.TempDir())
	cfg, err := config.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	_ = cfg.UpdateSettings(func(s *config.Settings) {
		s.EnableDXVK = true
	})

	runner := &proton.Runner{
		Name:       "GE-Proton10-32",
		Type:       "proton-ge",
		RootDir:    "/opt/keryx/runners/GE-Proton10-32",
		Wine64Path: "/opt/keryx/runners/GE-Proton10-32/files/bin/wine64",
	}

	env := proton.BuildEnv(cfg, runner)
	envJoined := strings.Join(env, "\n")

	if !strings.Contains(envJoined, "WINEARCH=win64") {
		t.Errorf("expected WINEARCH=win64 in env")
	}
	if !strings.Contains(envJoined, "d3d11,dxgi,d3d9,d3d10core=n,b") {
		t.Errorf("expected DXVK DLL overrides in env, got:\n%s", envJoined)
	}
	if !strings.Contains(envJoined, "GST_PLUGIN_SYSTEM_PATH_1_0=") {
		t.Errorf("expected GST_PLUGIN_SYSTEM_PATH_1_0 in env")
	}
	if !strings.Contains(envJoined, "icu,icuin,icuuc=n,b") {
		t.Errorf("expected icu,icuin,icuuc=n,b in WINEDLLOVERRIDES, got:\n%s", envJoined)
	}
	if strings.Contains(envJoined, "mscoree=") {
		t.Errorf("mscoree= must not be present in WINEDLLOVERRIDES (breaks fixup_imports_ilonly for System.Runtime.dll)")
	}
}

func TestPromotePendingAndBypassSplashStub(t *testing.T) {
	t.Setenv("KERYX_HOME", t.TempDir())
	cfg, err := config.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	proclaimRoot := filepath.Join(cfg.PrefixDir, "drive_c", "users", "steamuser", "AppData", "Local", "Proclaim")
	pendingDir := filepath.Join(proclaimRoot, "Pending")
	if err := os.MkdirAll(pendingDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create both the splash stub and Pending/Proclaim.exe
	_ = os.WriteFile(filepath.Join(proclaimRoot, "Proclaim.exe"), []byte("splash"), 0755)
	_ = os.WriteFile(filepath.Join(pendingDir, "Proclaim.exe"), []byte("dotnet10-apphost"), 0755)

	exe := proton.FindProclaimExe(cfg)
	expected := filepath.Join(proclaimRoot, "System", "Proclaim.exe")
	if exe != expected {
		t.Fatalf("expected FindProclaimExe to promote Pending->System and return %s, got %s", expected, exe)
	}
}

func TestBuildTTFWithFamilyName(t *testing.T) {
	// Construct a minimal valid TrueType header with 'head' and 'name' tables
	src := make([]byte, 12+2*16+64)
	copy(src[0:4], []byte{0, 1, 0, 0})
	src[5] = 2 // 2 tables
	copy(src[12:16], []byte("head"))
	src[23] = 44 // offset 44
	src[27] = 36 // length 36
	copy(src[28:32], []byte("name"))
	src[39] = 80 // offset 80
	src[43] = 16 // length 16

	patched, err := proton.BuildTTFWithFamilyName(src, "Segoe UI", "Regular")
	if err != nil {
		t.Fatalf("BuildTTFWithFamilyName failed: %v", err)
	}
	if !proton.TTFHasFamilyName(patched, "Segoe UI") {
		t.Fatalf("expected TTFHasFamilyName(patched, 'Segoe UI') to be true")
	}
	if proton.TTFHasFamilyName(patched, "Liberation Sans") {
		t.Fatalf("expected TTFHasFamilyName(patched, 'Liberation Sans') to be false")
	}
}
