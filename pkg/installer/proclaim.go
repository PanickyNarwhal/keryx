package installer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"keryx/pkg/config"
)

type ReleaseInfo struct {
	ConfirmationURL string `json:"confirmationUrl"`
	BootstrapperURL string `json:"bootstrapperUrl"`
	WebScriptURL    string `json:"webScriptUrl"`
	MSIURL          string `json:"msiUrl"`
	Version         string `json:"version"`
	ShortVersion    string `json:"shortVersion"`
	MinOSVersion    string `json:"minOsVersion"`
}

type ProgressFunc func(downloadedBytes, totalBytes int64, speedMBps float64, percent float64)

var (
	reWinURL         = regexp.MustCompile(`https://downloads\.logoscdn\.com/Proclaim/Installer/[A-Za-z0-9._/-]+\.(?:exe|msi)`)
	reWebScriptURL   = regexp.MustCompile(`https://downloads\.logoscdn\.com/Proclaim/Installer/([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)/ProclaimWebScript\.lbxdat`)
	reVersionComment = regexp.MustCompile(`The version is ([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)`)
	reShortVersion   = regexp.MustCompile(`version title is "([^"]+)"`)
	reMinOSVersion   = regexp.MustCompile(`minimum OS version of ([0-9.]+)`)
	reDownloadURI    = regexp.MustCompile(`download\|[^\r\n]*uri=([^\r\n|]+\.msi)`)
	reFeedVersion    = regexp.MustCompile(`<logos:version[^>]*>([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)</logos:version>`)
)

// ResolveFromDownloadConfirmation fetches https://proclaim.logos.com/download-confirmation?platform=win,
// extracts the Windows installer link (ProclaimSetup.exe), inspects its embedded ProclaimWebScript.lbxdat URL,
// and resolves the exact Proclaim-x64.msi URL and version metadata.
func ResolveFromDownloadConfirmation(ctx context.Context, cacheDir string, logf func(string, ...any)) (*ReleaseInfo, error) {
	info := &ReleaseInfo{
		ConfirmationURL: config.ProclaimDownloadConfirmationURL,
		BootstrapperURL: config.ProclaimBootstrapFallbackURL,
	}

	if logf != nil {
		logf("Checking Faithlife download page: %s", config.ProclaimDownloadConfirmationURL)
	}

	htmlBody, err := fetchBytes(ctx, config.ProclaimDownloadConfirmationURL)
	if err == nil {
		if m := reWinURL.Find(htmlBody); m != nil {
			info.BootstrapperURL = string(m)
			if logf != nil {
				logf("Discovered Windows installer URL from download-confirmation page: %s", info.BootstrapperURL)
			}
		}
	} else if logf != nil {
		logf("Warning: could not scrape download-confirmation page (%v), using fallback %s", err, info.BootstrapperURL)
	}

	// If the confirmation page points directly to an MSI, use it immediately
	if strings.HasSuffix(strings.ToLower(info.BootstrapperURL), ".msi") {
		info.MSIURL = info.BootstrapperURL
		return info, nil
	}

	// Download ProclaimSetup.exe to cacheDir to inspect its embedded WebScript URL
	_ = os.MkdirAll(cacheDir, 0755)
	bootstrapPath := filepath.Join(cacheDir, "ProclaimSetup.exe")
	if logf != nil {
		logf("Downloading bootstrapper %s...", info.BootstrapperURL)
	}
	if err := DownloadFile(ctx, info.BootstrapperURL, bootstrapPath, nil); err == nil {
		exeBytes, readErr := os.ReadFile(bootstrapPath)
		if readErr == nil {
			extractedText := extractASCIIAndUTF16Strings(exeBytes)
			if m := reWebScriptURL.FindStringSubmatch(extractedText); len(m) >= 2 {
				info.WebScriptURL = m[0]
				info.Version = m[1]
				if logf != nil {
					logf("Extracted ProclaimWebScript URL from ProclaimSetup.exe: %s (version %s)", info.WebScriptURL, info.Version)
				}
			}
		}
	} else if logf != nil {
		logf("Warning: failed to download ProclaimSetup.exe (%v), checking release feed...", err)
	}

	// Parse ProclaimWebScript.lbxdat if found
	if info.WebScriptURL != "" {
		scriptBytes, scriptErr := fetchBytes(ctx, info.WebScriptURL)
		if scriptErr == nil {
			scriptText := decodeUTF16OrUTF8(scriptBytes)
			if m := reVersionComment.FindStringSubmatch(scriptText); len(m) >= 2 {
				info.Version = m[1]
			}
			if m := reShortVersion.FindStringSubmatch(scriptText); len(m) >= 2 {
				info.ShortVersion = m[1]
			}
			if m := reMinOSVersion.FindStringSubmatch(scriptText); len(m) >= 2 {
				info.MinOSVersion = m[1]
			}
			msiFile := "Proclaim-x64.msi"
			if m := reDownloadURI.FindStringSubmatch(scriptText); len(m) >= 2 {
				msiFile = strings.TrimSpace(m[1])
			}
			baseURL := info.WebScriptURL[:strings.LastIndex(info.WebScriptURL, "/")+1]
			info.MSIURL = baseURL + msiFile
			if logf != nil {
				logf("Resolved 64-bit MSI from ProclaimWebScript.lbxdat: %s (min OS %s)", info.MSIURL, info.MinOSVersion)
			}
			return info, nil
		}
	}

	// Fallback: query Faithlife Proclaim XML update feed if WebScript extraction failed
	if info.Version == "" {
		feedBytes, feedErr := fetchBytes(ctx, config.ProclaimFeedFallbackURL)
		if feedErr == nil {
			if m := reFeedVersion.FindSubmatch(feedBytes); len(m) >= 2 {
				info.Version = string(m[1])
			}
		}
	}
	if info.Version == "" {
		info.Version = "4.22.0.0097"
	}
	parts := strings.Split(info.Version, ".")
	if len(parts) >= 2 && info.ShortVersion == "" {
		info.ShortVersion = parts[0] + "." + parts[1]
	}
	info.MSIURL = fmt.Sprintf("https://downloads.logoscdn.com/Proclaim/Installer/%s/Proclaim-x64.msi", info.Version)
	return info, nil
}

// DownloadFile downloads a remote URL to destPath using an atomic .part file and reports progress.
func DownloadFile(ctx context.Context, url, destPath string, onProgress ProgressFunc) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Keryx/"+config.AppVersion+" (Linux; x86_64)")

	client := &http.Client{
		Timeout: 0, // streaming large installers
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	total := resp.ContentLength

	// If destination already exists and matches remote Content-Length (> 1MB), reuse cached file
	if total > 1024*1024 {
		if st, err := os.Stat(destPath); err == nil && st.Size() == total {
			if onProgress != nil {
				onProgress(total, total, 0, 100.0)
			}
			return nil
		}
	}

	partPath := destPath + ".part"
	out, err := os.Create(partPath)
	if err != nil {
		return err
	}

	buf := make([]byte, 128*1024)
	var downloaded int64
	startTime := time.Now()
	lastReport := time.Time{}

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := out.Write(buf[:n]); wErr != nil {
				_ = out.Close()
				return wErr
			}
			downloaded += int64(n)
			now := time.Now()
			if onProgress != nil && (now.Sub(lastReport) >= 120*time.Millisecond || readErr == io.EOF) {
				lastReport = now
				elapsed := now.Sub(startTime).Seconds()
				speed := 0.0
				if elapsed > 0 {
					speed = (float64(downloaded) / (1024 * 1024)) / elapsed
				}
				pct := 0.0
				if total > 0 {
					pct = (float64(downloaded) / float64(total)) * 100.0
				}
				onProgress(downloaded, total, speed, pct)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = out.Close()
			return readErr
		}
	}

	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(partPath, destPath)
}

func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Keryx/"+config.AppVersion+" (Linux; x86_64)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func decodeUTF16OrUTF8(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		u16 := make([]uint16, (len(b)-2)/2)
		for i := range u16 {
			u16[i] = binary.LittleEndian.Uint16(b[2+i*2:])
		}
		return string(utf16.Decode(u16))
	}
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		u16 := make([]uint16, (len(b)-2)/2)
		for i := range u16 {
			u16[i] = binary.BigEndian.Uint16(b[2+i*2:])
		}
		return string(utf16.Decode(u16))
	}
	return string(b)
}

// extractASCIIAndUTF16Strings extracts printable ASCII and UTF-16LE sequences from binary PE data.
func extractASCIIAndUTF16Strings(data []byte) string {
	var sb strings.Builder
	// 1. Printable ASCII runs
	start := -1
	for i, c := range data {
		if c >= 0x20 && c <= 0x7E {
			if start == -1 {
				start = i
			}
		} else {
			if start != -1 && i-start >= 8 {
				sb.Write(data[start:i])
				sb.WriteByte('\n')
			}
			start = -1
		}
	}

	// 2. UTF-16LE ASCII runs (e.g. 'h', 0x00, 't', 0x00, ...)
	var u16Buf bytes.Buffer
	for i := 0; i+1 < len(data); i += 2 {
		lo, hi := data[i], data[i+1]
		if hi == 0 && lo >= 0x20 && lo <= 0x7E {
			u16Buf.WriteByte(lo)
		} else {
			if u16Buf.Len() >= 8 {
				sb.Write(u16Buf.Bytes())
				sb.WriteByte('\n')
			}
			u16Buf.Reset()
		}
	}
	if u16Buf.Len() >= 8 {
		sb.Write(u16Buf.Bytes())
		sb.WriteByte('\n')
	}
	return sb.String()
}
