package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
)

const (
	selfInstallPath  = "/usr/local/bin/vps-setup"
	launcherLibDir   = "/usr/local/lib/vps-setup"
	launchScriptPath = launcherLibDir + "/launch.sh"
	panelScriptPath  = launcherLibDir + "/panel.sh"
	desktopFileName  = "crystal-core-vps-setup.desktop"
	desktopFilePath  = "/usr/share/applications/" + desktopFileName
	autostartPath    = "/etc/xdg/autostart/crystal-core-vps-setup-panel.desktop"
	iconPath         = "/usr/share/icons/hicolor/512x512/apps/crystal-core-vps-setup.png"
	pixmapPath       = "/usr/share/pixmaps/crystal-core-vps-setup.png"
)

// installSelf копирует запущенный бинарник в /usr/local/bin/vps-setup, чтобы программу
// можно было запускать командой `sudo vps-setup` и значком на панели.
// Возвращает true, если копия была создана или обновлена.
func installSelf() (bool, error) {
	if dryRun {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return false, err
	}
	if exe == selfInstallPath {
		return false, nil
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return false, err
	}
	if cur, err := os.ReadFile(selfInstallPath); err == nil && bytes.Equal(cur, data) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(selfInstallPath), 0755); err != nil {
		return false, err
	}
	tmp := selfInstallPath + ".new"
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp, 0755); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, selfInstallPath)
}

// applyInSessionsFn запускает panel.sh внутри уже открытых XFCE-сессий (в том числе
// отключённых RDP-сессий): окружение берётся у процесса xfce4-panel каждого пользователя.
const applyInSessionsFn = `
apply_in_sessions() {
	local pid owner envs
	for pid in $(pgrep -x xfce4-panel || true); do
		owner=$(stat -c %U "/proc/$pid" 2>/dev/null) || continue
		envs=$(tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -E '^(DISPLAY|DBUS_SESSION_BUS_ADDRESS|XAUTHORITY|XDG_RUNTIME_DIR|HOME|XDG_CONFIG_HOME)=' || true)
		if [ -z "$envs" ]; then
			continue
		fi
		# shellcheck disable=SC2086
		timeout 150 runuser -u "$owner" -- env $envs "$PANEL_SCRIPT" "$1" || true
	done
}
`

func moduleLauncher(cfg *Config) error {
	if installed, err := installSelf(); err != nil {
		return fmt.Errorf("не удалось скопировать программу в %s: %w", selfInstallPath, err)
	} else if installed {
		appendLog("Программа скопирована в " + selfInstallPath + "\n")
	}

	files := []struct {
		embed string
		dest  string
		perm  os.FileMode
	}{
		{"configs/vps-setup-launch.sh", launchScriptPath, 0755},
		{"configs/vps-setup-panel.sh", panelScriptPath, 0755},
		{"configs/crystal-core-vps-setup.desktop", desktopFilePath, 0644},
		{"configs/crystal-core-vps-setup-panel.desktop", autostartPath, 0644},
	}
	for _, f := range files {
		if err := writeEmbeddedFile(f.embed, f.dest, f.perm); err != nil {
			return err
		}
	}
	for _, p := range []string{iconPath, pixmapPath} {
		if err := writeBytesFile(p, iconPNG, 0644, "assets/icon.png"); err != nil {
			return err
		}
	}

	return runBash(*cfg, applyInSessionsFn+`
		if command -v gtk-update-icon-cache >/dev/null 2>&1; then
			gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true
		fi
		if command -v update-desktop-database >/dev/null 2>&1; then
			update-desktop-database -q /usr/share/applications || true
		fi
		apply_in_sessions add
	`, "PANEL_SCRIPT="+panelScriptPath)
}

func uninstallLauncher(cfg *Config) error {
	err := runBash(*cfg, applyInSessionsFn+`
		if [ -x "$PANEL_SCRIPT" ]; then
			apply_in_sessions remove
		fi
	`, "PANEL_SCRIPT="+panelScriptPath)
	if err != nil {
		return err
	}

	// Пользователи без открытой сессии: убираем значок из сохранённой конфигурации панели.
	if err := cleanupPanelConfigs(); err != nil {
		return err
	}

	for _, p := range []string{desktopFilePath, autostartPath, iconPath, pixmapPath, launchScriptPath, panelScriptPath} {
		if dryRun {
			fmt.Println(dryStyle.Render("  [DRY-RUN] Удаление файла: " + p))
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if !dryRun {
		_ = os.Remove(launcherLibDir)
	}
	return runBash(*cfg, `
		if command -v gtk-update-icon-cache >/dev/null 2>&1; then
			gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true
		fi
		if command -v update-desktop-database >/dev/null 2>&1; then
			update-desktop-database -q /usr/share/applications || true
		fi
	`)
}

var launcherDirRe = regexp.MustCompile(`launcher-(\d+)$`)

// cleanupPanelConfigs убирает значок у пользователей, чья XFCE-сессия сейчас не запущена:
// удаляет каталог ярлыка и номер плагина из plugin-ids в xfce4-panel.xml. Если xfconfd
// пользователя работает, файл не трогаем — он перезаписал бы правку из памяти.
func cleanupPanelConfigs() error {
	homes, _ := filepath.Glob("/home/*")
	homes = append(homes, "/root")
	for _, home := range homes {
		conf := filepath.Join(home, ".config")
		marker := filepath.Join(conf, "vps-setup", "panel-added")
		matches, _ := filepath.Glob(filepath.Join(conf, "xfce4", "panel", "launcher-*", desktopFileName))
		if len(matches) == 0 && !fileExists(marker) {
			continue
		}
		if dryRun {
			fmt.Println(dryStyle.Render("  [DRY-RUN] Удаление значка из конфигурации панели в " + home))
			continue
		}
		uid := fileOwner(home)
		running := uid >= 0 && exec.Command("pgrep", "-u", strconv.Itoa(uid), "-x", "xfconfd").Run() == nil

		xmlPath := filepath.Join(conf, "xfce4", "xfconf", "xfce-perchannel-xml", "xfce4-panel.xml")
		for _, m := range matches {
			dir := filepath.Dir(m)
			if sm := launcherDirRe.FindStringSubmatch(dir); sm != nil && !running {
				if err := removePluginFromPanelXML(xmlPath, sm[1]); err != nil {
					return err
				}
			}
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
		_ = os.Remove(marker)
		_ = os.Remove(filepath.Dir(marker))
	}
	return nil
}

var pluginIDsBlockRe = regexp.MustCompile(`(?s)<property name="plugin-ids" type="array">.*?</property>`)

// removePluginFromPanelXML убирает плагин из всех списков plugin-ids. Неиспользуемое
// описание plugin-N панель просто игнорирует, поэтому его можно не трогать.
func removePluginFromPanelXML(path, id string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	valueRe := regexp.MustCompile(`\n[ \t]*<value type="int" value="` + regexp.QuoteMeta(id) + `"/>`)
	out := pluginIDsBlockRe.ReplaceAllStringFunc(string(data), func(block string) string {
		return valueRe.ReplaceAllString(block, "")
	})
	if out == string(data) {
		return nil
	}
	return os.WriteFile(path, []byte(out), 0644)
}

func fileOwner(path string) int {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
