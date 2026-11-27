package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/yourname/fixassist/internal/agentapi"
)

// Downloads serves the auto-fix daemon (cmd/fixassist), built for each
// platform into the server's image (Dockerfile), so the version users
// download always matches the server. Plus one-line install scripts:
//
//	GET /download/fixassist/{file}   a binary from DownloadFiles
//	GET /install/fixassist.ps1       Windows: irm <base>/install/fixassist.ps1 | iex
//	GET /install/fixassist.sh        macOS, Linux: curl -fsSL <base>/install/fixassist.sh | sh
type Downloads struct {
	Dir     string // FIXASSIST_DOWNLOADS: where the binaries are
	BaseURL string // this server's public URL, written into the scripts
}

// DownloadFile is one platform's build.
type DownloadFile struct{ File, Label string }

// DownloadFiles are the builds the Dockerfile makes, in the order the Setup
// page lists them.
var DownloadFiles = []DownloadFile{
	{"fixassist-windows-amd64.exe", "Windows"},
	{"fixassist-darwin-arm64", "Mac (Apple chip)"},
	{"fixassist-darwin-amd64", "Mac (Intel)"},
	{"fixassist-linux-amd64", "Linux"},
	{"fixassist-windows-arm64.exe", "Windows (ARM)"},
	{"fixassist-linux-arm64", "Linux (ARM)"},
}

func (d *Downloads) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /download/fixassist/{file}", d.binary)
	mux.HandleFunc("GET /install/fixassist.ps1", d.script(installPS1, "text/plain; charset=utf-8"))
	mux.HandleFunc("GET /install/fixassist.sh", d.script(installSH, "text/x-shellscript; charset=utf-8"))
}

func (d *Downloads) binary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	known := false
	for _, f := range DownloadFiles {
		known = known || f.File == name
	}
	if !known || d.Dir == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(d.Dir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	save := "fixassist"
	if strings.HasSuffix(name, ".exe") {
		save = "fixassist.exe"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+save+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (d *Downloads) script(body, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(renderScript(body, d.BaseURL, r.URL.Query().Get("pair"))))
	}
}

// installPS1 puts fixassist.exe in %LOCALAPPDATA%\fixassist and on the
// user's PATH. Re-running it updates fixassist, restarting the background
// watcher if it is installed.
const installPS1 = `# Fix Assistant: installs fixassist, the auto-fix helper.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$dir = Join-Path $env:LOCALAPPDATA 'fixassist'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$exe = Join-Path $dir 'fixassist.exe'
# A running background watcher holds the file: stop it while updating.
$installed = $false
try { schtasks /Query /TN FixAssist 2>$null | Out-Null; $installed = ($LASTEXITCODE -eq 0) } catch {}
if ($installed) { try { schtasks /End /TN FixAssist 2>$null | Out-Null } catch {}; Start-Sleep -Seconds 2 }
Invoke-WebRequest -UseBasicParsing -Uri "{{BASE}}/download/fixassist/fixassist-windows-$arch.exe" -OutFile "$exe.new"
Move-Item -Force "$exe.new" $exe
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath) { $userPath = '' }
if (($userPath -split ';') -notcontains $dir) {
  [Environment]::SetEnvironmentVariable('Path', (($userPath.TrimEnd(';') + ';' + $dir).TrimStart(';')), 'User')
}
if (($env:Path -split ';') -notcontains $dir) { $env:Path += ";$dir" }
if ($installed) { try { schtasks /Run /TN FixAssist 2>$null | Out-Null } catch {} }
Write-Host ''
& $exe version
Write-Host "Installed to $exe"
$pair = '{{PAIR}}'
if ($pair) {
  Write-Host ''
  & $exe pair --server '{{BASE}}' --code $pair --repo (Get-Location).Path
  if ($LASTEXITCODE -eq 0) { & $exe install }
} else {
  Write-Host 'Next, copy the install command from the Setup page and run it in your app''s code folder.'
}
`

// installSH puts fixassist in ~/.local/bin (or $FIXASSIST_DIR).
const installSH = `#!/bin/sh
# Fix Assistant: installs fixassist, the auto-fix helper.
set -eu
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) echo "fixassist doesn't support $os; on Windows use PowerShell: irm {{BASE}}/install/fixassist.ps1 | iex" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "fixassist doesn't support $(uname -m) processors" >&2; exit 1 ;;
esac
dir="${FIXASSIST_DIR:-$HOME/.local/bin}"
mkdir -p "$dir"
curl -fsSL "{{BASE}}/download/fixassist/fixassist-$os-$arch" -o "$dir/.fixassist.new"
chmod +x "$dir/.fixassist.new"
mv -f "$dir/.fixassist.new" "$dir/fixassist"
"$dir/fixassist" version
echo "Installed to $dir/fixassist"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add it to your PATH first:  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile && . ~/.profile" ;;
esac
pair='{{PAIR}}'
if [ -n "$pair" ]; then
  echo
  if "$dir/fixassist" pair --server '{{BASE}}' --code "$pair" --repo "$PWD"; then
    "$dir/fixassist" install
  fi
else
  echo 'Next, copy the install command from the Setup page and run it in your app'"'"'s code folder.'
fi
`

// renderScript fills in the server's URL and, with ?pair=, the one-time
// code: the script then connects the current folder and starts the watcher.
// Only the code's own characters get into the script.
func renderScript(body, base, pair string) string {
	code := agentapi.NormalizePairCode(pair)
	if len(code) == 10 {
		code = code[:5] + "-" + code[5:]
	} else {
		code = ""
	}
	return strings.NewReplacer("{{BASE}}", strings.TrimRight(base, "/"), "{{PAIR}}", code).Replace(body)
}
