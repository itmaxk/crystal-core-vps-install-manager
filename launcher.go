package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	panelItemPrefix  = "crystal-core-"
)

// panelRegistryDir — список программ, значки которых должны быть на нижней панели XFCE.
// panel.sh sync приводит панель каждого пользователя в соответствие с этим каталогом.
var panelRegistryDir = "/usr/share/vps-setup/panel"

// panelItem — программа, которую выносим на панель (и при необходимости в меню приложений).
type panelItem struct {
	Name    string // имя .desktop-файла, начинается с crystal-core-
	Desktop string // содержимое .desktop-файла
	SVGIcon string // своя иконка (SVG), если у программы её нет
	Menu    bool   // добавить и в меню приложений (/usr/share/applications)
	Wrapper string // скрипт запуска (кладётся в appsDir под именем без .desktop)
}

// appsDir — скрипты запуска консольных программ. Значок указывает на скрипт без
// аргументов: так команда одинаково работает в любом терминале (Kitty, xfce4-terminal).
const appsDir = launcherLibDir + "/apps"

func (p panelItem) wrapperPath() string {
	return appsDir + "/" + strings.TrimPrefix(p.iconName(), panelItemPrefix)
}

func (p panelItem) iconName() string { return strings.TrimSuffix(p.Name, ".desktop") }

func (p panelItem) svgPath() string {
	return "/usr/share/icons/hicolor/scalable/apps/" + p.iconName() + ".svg"
}

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

const refreshDesktopCachesFn = `
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
	gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true
fi
if command -v update-desktop-database >/dev/null 2>&1; then
	update-desktop-database -q /usr/share/applications || true
fi
`

// registerPanelItems добавляет программы в список значков панели и сразу применяет его
// в открытых XFCE-сессиях (в остальных — при следующем входе).
func registerPanelItems(cfg Config, items []panelItem) error {
	if len(items) == 0 {
		return nil
	}
	for _, f := range []struct {
		embed, dest string
		perm        os.FileMode
	}{
		{"configs/vps-setup-panel.sh", panelScriptPath, 0755},
		{"configs/crystal-core-vps-setup-panel.desktop", autostartPath, 0644},
	} {
		if err := writeEmbeddedFile(f.embed, f.dest, f.perm); err != nil {
			return err
		}
	}
	for _, it := range items {
		if err := writeBytesFile(filepath.Join(panelRegistryDir, it.Name), []byte(it.Desktop), 0644, it.Name); err != nil {
			return err
		}
		if it.Menu {
			if err := writeBytesFile(filepath.Join("/usr/share/applications", it.Name), []byte(it.Desktop), 0644, it.Name); err != nil {
				return err
			}
		}
		if it.SVGIcon != "" {
			if err := writeBytesFile(it.svgPath(), []byte(it.SVGIcon), 0644, it.Name+" (иконка)"); err != nil {
				return err
			}
		}
		if it.Wrapper != "" {
			if err := writeBytesFile(it.wrapperPath(), []byte(it.Wrapper), 0755, it.Name+" (запуск)"); err != nil {
				return err
			}
		}
	}
	return runBash(cfg, applyInSessionsFn+refreshDesktopCachesFn+`
		apply_in_sessions sync
	`, "PANEL_SCRIPT="+panelScriptPath)
}

// unregisterPanelItems убирает программы из списка значков, а их значки — с панелей:
// в открытых сессиях сразу, у остальных пользователей — правкой сохранённой конфигурации.
// Когда список пустеет, удаляются и panel.sh с автозапуском.
func unregisterPanelItems(cfg Config, items []panelItem) error {
	var names []string
	for _, it := range items {
		names = append(names, it.Name)
		for _, p := range []string{filepath.Join(panelRegistryDir, it.Name), filepath.Join("/usr/share/applications", it.Name), it.svgPath(), it.wrapperPath()} {
			if dryRun {
				if fileExists(p) {
					fmt.Println(dryStyle.Render("  [DRY-RUN] Удаление файла: " + p))
				}
				continue
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}

	if err := runBash(cfg, applyInSessionsFn+refreshDesktopCachesFn+`
		if [ -x "$PANEL_SCRIPT" ]; then
			apply_in_sessions sync
		fi
	`, "PANEL_SCRIPT="+panelScriptPath); err != nil {
		return err
	}
	if err := cleanupPanelConfigs(names); err != nil {
		return err
	}

	remaining, _ := filepath.Glob(filepath.Join(panelRegistryDir, panelItemPrefix+"*.desktop"))
	if len(remaining) == 0 && !dryRun {
		for _, p := range []string{panelScriptPath, autostartPath} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		_ = os.Remove(panelRegistryDir)
		_ = os.Remove(filepath.Dir(panelRegistryDir))
		_ = os.Remove(launcherLibDir)
	}
	if !dryRun {
		_ = os.Remove(appsDir)
		_ = os.Remove(launcherLibDir)
	}
	return nil
}

// --- Значки конкретных программ ---

func vpsSetupPanelItem() panelItem {
	data, _ := configFS.ReadFile("configs/crystal-core-vps-setup.desktop")
	return panelItem{Name: desktopFileName, Desktop: string(data)}
}

func kittyPanelItem() panelItem {
	return panelItem{
		Name: "crystal-core-kitty.desktop",
		Desktop: `[Desktop Entry]
Version=1.0
Type=Application
Name=Kitty
Comment=Быстрый терминал с поддержкой GPU
Exec=kitty
Icon=kitty
Terminal=false
Categories=System;TerminalEmulator;
`,
	}
}

// AI-утилиты консольные: значок открывает терминал со скриптом запуска (appsDir).
type aiTool struct {
	id, bin, title, comment, letters, color string
}

var aiTools = []aiTool{
	{"claude", "claude", "Claude Code", "ИИ-ассистент для разработки в терминале", "Cl", "#D97757"},
	{"codex", "codex", "Codex CLI", "ИИ-агент для кода в терминале", "Cx", "#1F2937"},
	{"opencode", "opencode", "OpenCode", "Открытый ИИ-агент для кода в терминале", "OC", "#4B5563"},
	{"gemini", "gemini", "Gemini CLI", "ИИ-ассистент в терминале", "Ge", "#3B6FE0"},
	{"aider", "aider", "Aider", "Парное программирование с ИИ в git-репозитории", "Ai", "#0E9F8E"},
}

func (t aiTool) panelItem() panelItem {
	name := panelItemPrefix + t.id + ".desktop"
	it := panelItem{
		Name:    name,
		Menu:    true,
		SVGIcon: letterTileSVG(t.letters, t.color),
		Wrapper: toolWrapper(t.title, t.bin),
	}
	it.Desktop = fmt.Sprintf(`[Desktop Entry]
Version=1.0
Type=Application
Name=%s
Comment=%s
Exec=%s
Icon=%s
Terminal=true
Categories=Development;
`, t.title, t.comment, it.wrapperPath(), it.iconName())
	return it
}

// toolWrapper — скрипт запуска консольной программы из меню/панели: login-shell
// (подхватывает ~/.local/bin, куда ставятся uv и aider), понятное сообщение, если
// программы нет, и пауза перед закрытием окна, если она завершилась с ошибкой.
func toolWrapper(title, bin string) string {
	return fmt.Sprintf(`#!/bin/bash -l
# Crystal-Core VPS Install Manager: запуск %[1]s из меню и с панели XFCE.
export PATH="$HOME/.local/bin:$PATH"
cd "$HOME" || exit 1
if ! command -v %[2]s >/dev/null 2>&1; then
	echo "%[1]s не найден. Установите модуль ai_cli: sudo vps-setup"
	read -r -p "Нажмите Enter, чтобы закрыть окно..." _
	exit 1
fi
%[2]s "$@"
rc=$?
if [ "$rc" -ne 0 ]; then
	echo
	echo "%[1]s завершился с кодом $rc."
	read -r -p "Нажмите Enter, чтобы закрыть окно..." _
fi
exit "$rc"
`, title, bin)
}

// repairKnownIssues чинит то, что поставили версии до 1.2.1, при любом запуске от root:
//   - helper Kitty передавал терминалу команду одной строкой («kitty -- "%s"»), и значки
//     консольных программ падали с «failed to launch child: ... No such file or directory»;
//   - значки AI-утилит запускали «bash -lc <программа>» — переводим их на скрипты запуска.
func repairKnownIssues(cfg Config) {
	if dryRun || os.Geteuid() != 0 {
		return
	}
	repaired := false
	homes, _ := filepath.Glob("/home/*")
	for _, home := range append(homes, "/root") {
		p := filepath.Join(home, ".local", "share", "xfce4", "helpers", "kitty.desktop")
		data, err := os.ReadFile(p)
		if err != nil || !bytes.Contains(data, []byte(`kitty -- "%s"`)) {
			continue
		}
		fixed := bytes.ReplaceAll(data, []byte(`X-XFCE-CommandsWithParameter=kitty -- "%s"`), []byte(`X-XFCE-CommandsWithParameter=kitty %s`))
		if os.WriteFile(p, fixed, 0644) == nil {
			repaired = true
		}
	}

	var stale []panelItem
	for _, t := range aiTools {
		if fileContains(filepath.Join(panelRegistryDir, t.panelItem().Name), "Exec=bash -lc") {
			stale = append(stale, t.panelItem())
		}
	}
	if len(stale) > 0 {
		currentModule = "ai_cli"
		if registerPanelItems(cfg, stale) == nil {
			repaired = true
		}
	}
	if repaired {
		appendLog("Исправлен запуск программ со значков панели (helper Kitty / скрипты запуска AI-утилит)\n")
		fmt.Println(dimStyle.Render("Исправлен запуск программ со значков на панели XFCE."))
	}

	// Иконка программы сменилась (1.2.2): обновляем уже установленную копию.
	iconChanged := false
	for _, p := range []string{iconPath, pixmapPath} {
		data, err := os.ReadFile(p)
		if err != nil || bytes.Equal(data, iconPNG) {
			continue
		}
		if os.WriteFile(p, iconPNG, 0644) == nil {
			iconChanged = true
		}
	}
	if iconChanged {
		_ = runBash(cfg, refreshDesktopCachesFn)
		appendLog("Обновлена иконка программы\n")
	}
}

// letterTileSVG — простая иконка-плитка с буквами (без чужих логотипов).
func letterTileSVG(letters, color string) string {
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="128" height="128" viewBox="0 0 128 128">
  <rect x="4" y="4" width="120" height="120" rx="28" fill="%s"/>
  <rect x="4" y="4" width="120" height="60" rx="28" fill="#ffffff" fill-opacity="0.08"/>
  <text x="64" y="82" text-anchor="middle" font-family="DejaVu Sans, Liberation Sans, sans-serif" font-size="52" font-weight="bold" fill="#ffffff">%s</text>
</svg>
`, color, letters)
}

// installedAIToolItems — значки только для реально установленных утилит.
func installedAIToolItems(cfg Config) []panelItem {
	var items []panelItem
	for _, t := range aiTools {
		if dryRun || hasBin(t.bin) || fileExists(filepath.Join(cfg.TargetHome, ".local", "bin", t.bin)) {
			items = append(items, t.panelItem())
		}
	}
	return items
}

func allAIToolItems() []panelItem {
	var items []panelItem
	for _, t := range aiTools {
		items = append(items, t.panelItem())
	}
	return items
}

// --- Модуль panel_launcher: сама программа в меню и на панели ---

func moduleLauncher(cfg *Config) error {
	if installed, err := installSelf(); err != nil {
		return fmt.Errorf("не удалось скопировать программу в %s: %w", selfInstallPath, err)
	} else if installed {
		appendLog("Программа скопирована в " + selfInstallPath + "\n")
	}
	if err := writeEmbeddedFile("configs/vps-setup-launch.sh", launchScriptPath, 0755); err != nil {
		return err
	}
	if err := writeEmbeddedFile("configs/crystal-core-vps-setup.desktop", desktopFilePath, 0644); err != nil {
		return err
	}
	for _, p := range []string{iconPath, pixmapPath} {
		if err := writeBytesFile(p, iconPNG, 0644, "assets/icon.png"); err != nil {
			return err
		}
	}
	return registerPanelItems(*cfg, []panelItem{vpsSetupPanelItem()})
}

func uninstallLauncher(cfg *Config) error {
	if err := unregisterPanelItems(*cfg, []panelItem{vpsSetupPanelItem()}); err != nil {
		return err
	}
	for _, p := range []string{desktopFilePath, iconPath, pixmapPath, launchScriptPath} {
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
	return runBash(*cfg, refreshDesktopCachesFn)
}

var launcherDirRe = regexp.MustCompile(`launcher-(\d+)$`)

// cleanupPanelConfigs убирает значки names у пользователей, чья XFCE-сессия сейчас не
// запущена: удаляет каталог ярлыка и номер плагина из plugin-ids в xfce4-panel.xml.
// Если xfconfd пользователя работает, файл не трогаем — он перезаписал бы правку из памяти.
func cleanupPanelConfigs(names []string) error {
	homes, _ := filepath.Glob("/home/*")
	homes = append(homes, "/root")
	for _, home := range homes {
		conf := filepath.Join(home, ".config")
		var matches []string
		for _, n := range names {
			m, _ := filepath.Glob(filepath.Join(conf, "xfce4", "panel", "launcher-*", n))
			matches = append(matches, m...)
		}
		if len(matches) == 0 {
			continue
		}
		if dryRun {
			fmt.Println(dryStyle.Render("  [DRY-RUN] Удаление значков из конфигурации панели в " + home))
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
		for _, n := range names {
			_ = os.Remove(filepath.Join(conf, "vps-setup", "panel", n))
		}
		if strings.Contains(strings.Join(names, " "), desktopFileName) {
			_ = os.Remove(filepath.Join(conf, "vps-setup", "panel-added"))
		}
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
