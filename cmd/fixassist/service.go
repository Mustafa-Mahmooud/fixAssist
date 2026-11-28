package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"
)

// Running watch in the background: started at login with no window and
// restarted when it stops. Windows: a Task Scheduler task. macOS: a launchd
// agent. Linux: a systemd user service.

const serviceName = "FixAssist"

func logFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "fixassist", "fixassist.log"), nil
}

func cmdInstall() error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Projects) == 0 {
		return errors.New("no apps connected: run fixassist add first")
	}
	for _, tool := range []string{"git", "gh", "claude"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s isn't installed or isn't on PATH; install it first, then run fixassist install again", tool)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	log, err := logFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		return err
	}
	ctx := context.Background()
	switch runtime.GOOS {
	case "windows":
		err = installWindows(ctx, exe, log)
	case "darwin":
		err = installMac(ctx, exe, log)
	case "linux":
		err = installLinux(ctx, exe, log)
	default:
		return fmt.Errorf("install isn't supported on %s: run fixassist watch yourself", runtime.GOOS)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Installed. fixassist now runs in the background, starts by itself when you log in, and restarts if it stops.\nLog: %s\nCheck it with: fixassist status\n", log)
	fmt.Println("If you move or update fixassist, run fixassist install again.")
	return nil
}

func cmdUninstall() error {
	ctx := context.Background()
	var err error
	switch runtime.GOOS {
	case "windows":
		_, _ = run(ctx, "", nil, "schtasks", "/End", "/TN", serviceName)
		_, err = run(ctx, "", nil, "schtasks", "/Delete", "/TN", serviceName, "/F")
	case "darwin":
		p, perr := plistPath()
		if perr != nil {
			return perr
		}
		_, _ = run(ctx, "", nil, "launchctl", "unload", p)
		err = os.Remove(p)
	case "linux":
		_, _ = run(ctx, "", nil, "systemctl", "--user", "disable", "--now", "fixassist.service")
		p, perr := unitPath()
		if perr != nil {
			return perr
		}
		err = os.Remove(p)
		_, _ = run(ctx, "", nil, "systemctl", "--user", "daemon-reload")
	default:
		return fmt.Errorf("not supported on %s", runtime.GOOS)
	}
	if err != nil {
		return fmt.Errorf("it doesn't look installed: %v", err)
	}
	fmt.Println("Uninstalled: fixassist no longer runs in the background.")
	return nil
}

func cmdStatus() error {
	ctx := context.Background()
	var state string
	switch runtime.GOOS {
	case "windows":
		out, err := run(ctx, "", nil, "schtasks", "/Query", "/TN", serviceName, "/FO", "LIST")
		if err != nil {
			state = "not installed (run fixassist install)"
		} else {
			state = "installed"
			for _, l := range lines(out) {
				if s, ok := strings.CutPrefix(l, "Status:"); ok {
					state += ", " + strings.ToLower(strings.TrimSpace(s))
				}
			}
		}
	case "darwin":
		if _, err := run(ctx, "", nil, "launchctl", "list", "com.fixassist.watch"); err != nil {
			state = "not installed (run fixassist install)"
		} else {
			state = "installed, running"
		}
	case "linux":
		out, _ := run(ctx, "", nil, "systemctl", "--user", "is-active", "fixassist.service")
		if s := strings.TrimSpace(string(out)); s == "active" {
			state = "installed, running"
		} else {
			state = "not running (" + s + "); run fixassist install"
		}
	default:
		state = "unknown on " + runtime.GOOS
	}
	fmt.Println("Background watcher:", state)
	switch pid, ver := runningWatcher(); {
	case pid == "":
		fmt.Println("Running: no fixassist watch is running on this computer.")
	case ver == "":
		fmt.Printf("Running: a fixassist older than 0.4.0. This command is %s: run fixassist install to switch to it.\n", version)
	default:
		fmt.Printf("Running: fixassist %s (pid %s). This command is %s.\n", ver, pid, version)
	}
	log, err := logFile()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(log)
	if err != nil {
		fmt.Println("No log yet:", log)
		return nil
	}
	ls := lines(b)
	fmt.Printf("Log (%s), latest lines:\n", log)
	for _, l := range ls[max(0, len(ls)-10):] {
		fmt.Println("  " + l)
	}
	return nil
}

// --- Windows: Task Scheduler ------------------------------------------------

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// taskXML is a logon task with no time limit that keeps running on battery
// and restarts every minute when it stops. conhost --headless runs the
// console program without a window.
func taskXML(userID, exe, log string) string {
	args := fmt.Sprintf(`--headless "%s" watch --log "%s"`, exe, log)
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Fix Assistant auto-fix: fixes approved issues with Claude Code.</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + xmlEscape(userID) + `</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>` + xmlEscape(userID) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author"><Exec><Command>conhost.exe</Command><Arguments>` + xmlEscape(args) + `</Arguments></Exec></Actions>
</Task>
`
}

// utf16File is what schtasks /XML reads reliably: UTF-16LE with a BOM.
func utf16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, c := range u {
		b[2+2*i], b[3+2*i] = byte(c), byte(c>>8)
	}
	return b
}

func installWindows(ctx context.Context, exe, log string) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("fixassist-task-%d.xml", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, utf16File(taskXML(u.Username, exe, log)), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, _ = run(ctx, "", nil, "schtasks", "/End", "/TN", serviceName) // reinstall: stop the old one
	if _, err := run(ctx, "", nil, "schtasks", "/Create", "/TN", serviceName, "/XML", tmp, "/F"); err != nil {
		return fmt.Errorf("couldn't create the scheduled task: %v", err)
	}
	if _, err := run(ctx, "", nil, "schtasks", "/Run", "/TN", serviceName); err != nil {
		return fmt.Errorf("created the task but couldn't start it: %v", err)
	}
	return nil
}

// --- macOS: launchd -----------------------------------------------------------

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.fixassist.watch.plist"), nil
}

func installMac(ctx context.Context, exe, log string) error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	// launchd starts programs with a minimal PATH: keep this shell's, so
	// claude, git and gh are found.
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.fixassist.watch</string>
  <key>ProgramArguments</key><array><string>` + xmlEscape(exe) + `</string><string>watch</string><string>--log</string><string>` + xmlEscape(log) + `</string></array>
  <key>EnvironmentVariables</key><dict><key>PATH</key><string>` + xmlEscape(os.Getenv("PATH")) + `</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>60</integer>
</dict></plist>
`
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	_, _ = run(ctx, "", nil, "launchctl", "unload", p)
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return err
	}
	if _, err := run(ctx, "", nil, "launchctl", "load", "-w", p); err != nil {
		return fmt.Errorf("launchctl load: %v", err)
	}
	return nil
}

// --- Linux: systemd user service ---------------------------------------------

func unitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", "fixassist.service"), nil
}

func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s) + `"`
}

func installLinux(ctx context.Context, exe, log string) error {
	p, err := unitPath()
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=Fix Assistant auto-fix: fixes approved issues with Claude Code
After=network-online.target

[Service]
ExecStart=%s watch --log %s
Environment=%s
Restart=always
RestartSec=60

[Install]
WantedBy=default.target
`, systemdQuote(exe), systemdQuote(log), systemdQuote("PATH="+os.Getenv("PATH")))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "fixassist.service"}, {"--user", "restart", "fixassist.service"}} {
		if _, err := run(ctx, "", nil, "systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %s: %v", strings.Join(args, " "), err)
		}
	}
	// Without lingering, user services stop at logout; it's fine for a
	// laptop, so only suggest it.
	fmt.Println("Tip: to keep it running while you're logged out: loginctl enable-linger $USER")
	return nil
}
