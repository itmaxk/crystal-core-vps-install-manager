package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// --- Выбор и проверка того, что удалять ---

// removableModules — модули, которые установлены (или от которых остались следы) и
// которые программа умеет удалять. Порядок — как в меню установки.
func removableModules(cfg Config) []string {
	var result []string
	for _, m := range getModuleDefs() {
		if !m.Removable {
			continue
		}
		// Только то, что поставила сама программа (есть её отметка или учёт), — даже если
		// компонент установлен в системе, но ставили его вручную, он не предлагается.
		installed := false
		if m.ID == "create_user" {
			installed = len(existingCreatedUsers()) > 0
		} else {
			installed = hasStateFor(m.ID)
		}
		if installed {
			result = append(result, m.ID)
		}
	}
	return result
}

func hasStateFor(mod string) bool {
	matches, _ := filepath.Glob(filepath.Join(stateDir, mod+".*"))
	return len(matches) > 0
}

// parseRemoveModules разбирает --remove: список модулей или all.
func parseRemoveModules(raw string, cfg Config) ([]string, error) {
	if strings.TrimSpace(raw) == "all" {
		return orderForRemoval(removableModules(cfg)), nil
	}
	mods, err := parseOnlyModules(raw)
	if err != nil {
		return nil, err
	}
	installed := removableModules(cfg)
	for _, id := range mods {
		if m, _ := moduleDef(id); !m.Removable {
			return nil, fmt.Errorf("модуль %s удалить нельзя: %s", id, notRemovableReason(id))
		}
		if !contains(installed, id) {
			return nil, fmt.Errorf("модуль %s не устанавливался этой программой — удалять нечего (вручную установленное программа не трогает)", id)
		}
	}
	return orderForRemoval(mods), nil
}

func notRemovableReason(id string) string {
	switch id {
	case "update":
		return "это обновления и базовые системные утилиты"
	case "upgrade_ubuntu":
		return "переход на новую версию Ubuntu не откатывается"
	}
	return "не поддерживается"
}

// orderForRemoval — обратный порядок установки: сначала то, что зависит от остального.
func orderForRemoval(mods []string) []string {
	ordered := orderModules(mods)
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	return ordered
}

// existingCreatedUsers — пользователи, созданные модулем create_user и ещё существующие.
func existingCreatedUsers() []string {
	data, err := os.ReadFile(filepath.Join(stateDir, "create_user.users"))
	if err != nil {
		return nil
	}
	var users []string
	for _, line := range strings.Split(string(data), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || contains(users, name) {
			continue
		}
		if _, err := user.Lookup(name); err == nil {
			users = append(users, name)
		}
	}
	return users
}

// otherAdminWithKeys: останется ли после удаления пользователей хотя бы один участник
// группы sudo с SSH-ключом — иначе на сервер будет не зайти.
func otherAdminWithKeys(excluded []string) bool {
	data, err := os.ReadFile(groupFile)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 4 || (f[0] != "sudo" && f[0] != "admin") {
			continue
		}
		for _, member := range strings.Split(f[3], ",") {
			member = strings.TrimSpace(member)
			if member == "" || member == "root" || contains(excluded, member) {
				continue
			}
			if u, err := user.Lookup(member); err == nil && hasValidKeyFile(filepath.Join(u.HomeDir, ".ssh", "authorized_keys")) {
				return true
			}
		}
	}
	return false
}

func validateRemovePlan(cfg *Config) error {
	if len(cfg.Modules) == 0 {
		return fmt.Errorf("не выбрано ни одного модуля для удаления")
	}
	installed := removableModules(*cfg)
	for _, id := range cfg.Modules {
		m, ok := moduleDef(id)
		if !ok || !m.Removable {
			return fmt.Errorf("модуль %s удалить нельзя: %s", id, notRemovableReason(id))
		}
		if !contains(installed, id) {
			return fmt.Errorf("модуль %s не устанавливался этой программой — удалять нечего", id)
		}
	}
	if contains(cfg.Modules, "create_user") {
		users := existingCreatedUsers()
		if len(users) == 0 {
			return fmt.Errorf("create_user: программа не создавала пользователей (или они уже удалены)")
		}
		if contains(users, cfg.TargetUser) {
			return fmt.Errorf("create_user: пользователь %s — это вы (программа запущена от его имени через sudo). Войдите под другим администратором (например, ubuntu) и повторите удаление", cfg.TargetUser)
		}
		if !otherAdminWithKeys(users) {
			return dryRunTolerant(fmt.Errorf("create_user: после удаления %s не останется другого администратора (группа sudo) с SSH-ключом — доступ к серверу будет потерян", strings.Join(users, ", ")))
		}
	}
	return nil
}

// removalSummary — текст подтверждения: что будет удалено и о чём стоит знать.
func removalSummary(mods []string) string {
	if len(mods) == 0 {
		return "Ничего не выбрано."
	}
	ordered := orderForRemoval(mods)
	var b strings.Builder
	b.WriteString("Будет удалено (в этом порядке):\n")
	for _, id := range ordered {
		b.WriteString("  • " + moduleTitle(id) + " (" + id + ")\n")
	}
	var notes []string
	if contains(mods, "create_user") {
		notes = append(notes, "пользователи "+strings.Join(existingCreatedUsers(), ", ")+" будут удалены вместе с домашними каталогами и всеми файлами")
	}
	if contains(mods, "xfce") || contains(mods, "xrdp") {
		notes = append(notes, "открытые RDP-сессии будут завершены")
	}
	if contains(mods, "ssh_hardening") {
		notes = append(notes, "вернутся исходные настройки SSH (порт, вход по паролю и под root — как было до установки)")
	}
	if contains(mods, "fail2ban_ufw") {
		notes = append(notes, "файрвол UFW вернётся в прежнее состояние (если был выключен — будет выключен)")
	}
	if contains(mods, "docker") {
		notes = append(notes, "образы, контейнеры и тома Docker (/var/lib/docker) сохранятся")
	}
	if contains(mods, "ollama") {
		notes = append(notes, "скачанные модели Ollama будут удалены")
	}
	if len(notes) > 0 {
		b.WriteString("\nОбратите внимание:\n")
		for _, n := range notes {
			b.WriteString("  ⚠ " + n + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// runRemove удаляет модули в обратном порядке установки; ошибка в одном не останавливает остальные.
func runRemove(cfg Config) {
	cfg.Modules = orderForRemoval(cfg.Modules)
	fmt.Println(titleStyle.Render("Удаление: " + strings.Join(cfg.Modules, " → ")))
	appendLog(fmt.Sprintf("\n=== Crystal-Core VPS Install Manager %s — удаление — %s ===\nМодули: %s\n",
		Version, time.Now().Format(time.RFC3339), strings.Join(cfg.Modules, ",")))

	var failed []string
	for i, mod := range cfg.Modules {
		title := fmt.Sprintf("[%d/%d] Удаление: %s (%s)", i+1, len(cfg.Modules), moduleTitle(mod), mod)
		appendLog(fmt.Sprintf("\n### remove %s — %s\n", mod, time.Now().Format(time.RFC3339)))
		err := withSpinner(title, func() error { return uninstallModule(mod, &cfg) })
		if err != nil {
			fmt.Println(errStyle.Render(fmt.Sprintf("✖ %s — ошибка: %v", title, err)))
			appendLog(fmt.Sprintf("!!! Ошибка удаления %s: %v\n", mod, err))
			failed = append(failed, mod)
			continue
		}
		fmt.Println(okStyle.Render("✓ " + title))
		clearModuleState(mod)
	}

	fmt.Println()
	if len(failed) > 0 {
		fmt.Println(errStyle.Render("Удаление завершено с ошибками в модулях: " + strings.Join(failed, ", ")))
		fmt.Println(warnStyle.Render("Подробности смотрите в " + logPath + ", затем повторите: sudo vps-setup --remove " + strings.Join(failed, ",")))
	} else {
		fmt.Println(okStyle.Render("Готово! Выбранные компоненты удалены."))
	}
	if contains(cfg.Modules, "ssh_hardening") && !contains(failed, "ssh_hardening") {
		fmt.Println(warnStyle.Render(fmt.Sprintf("⚠ SSH теперь слушает порт %s. Не закрывайте текущую сессию, пока не проверите вход в новом окне.", strings.Join(detectSSHPorts(), ","))))
	}
	if contains(cfg.Modules, "docker") && !contains(failed, "docker") {
		fmt.Println(dimStyle.Render("Данные Docker сохранены. Удалить их: sudo rm -rf /var/lib/docker /var/lib/containerd"))
	}
	if contains(cfg.Modules, "panel_launcher") || len(removableModules(cfg)) == 0 {
		fmt.Println(dimStyle.Render("Сама программа осталась в " + selfInstallPath + " — удалить её: sudo rm " + selfInstallPath))
	}
	if len(failed) > 0 {
		os.Exit(1)
	}
}

// --- Удаление модулей ---

func uninstallModule(mod string, cfg *Config) error {
	currentModule = mod
	switch mod {
	case "create_user":
		users := existingCreatedUsers()
		if contains(users, cfg.TargetUser) {
			return fmt.Errorf("нельзя удалить пользователя %s, от имени которого запущена программа", cfg.TargetUser)
		}
		return runBash(*cfg, `
			for u in $DEL_USERS; do
				pkill -KILL -u "$u" 2>/dev/null || true
				sleep 1
				rc=0
				userdel -r "$u" || rc=$?
				# 12 — не удалось удалить домашний каталог/почтовый ящик: сам пользователь удалён.
				if [ "$rc" -ne 0 ] && [ "$rc" -ne 12 ]; then
					exit "$rc"
				fi
				sed -i "/^${u}\$/d" "$STATE_DIR/create_user.users"
			done
			if [ ! -s "$STATE_DIR/create_user.users" ]; then
				rm -f "$STATE_DIR/create_user.users"
			fi
		`, "DEL_USERS="+strings.Join(users, " "))

	case "swap":
		return runBash(*cfg, `
			if swapon --show=NAME --noheadings | grep -qx /swapfile; then
				swapoff /swapfile
			fi
			sed -i '\|^/swapfile |d' /etc/fstab
			rm -f /swapfile
		`)

	case "xfce":
		return runBash(*cfg, `
			apt_purge_recorded
			TARGET_FILE="$STATE_DIR/xfce.default-target"
			if [ -s "$TARGET_FILE" ]; then
				systemctl set-default "$(cat "$TARGET_FILE")" || true
			fi
			rm -f "$TARGET_FILE"
		`)

	case "xrdp":
		err := forEachModuleUser(mod, *cfg, func(u Config) error {
			return runBash(u, `
				if [ -f "$TARGET_HOME/.xsession" ] && [ "$(cat "$TARGET_HOME/.xsession")" = "xfce4-session" ]; then
					rm -f "$TARGET_HOME/.xsession"
				fi
			`)
		})
		if err != nil {
			return err
		}
		err = runBash(*cfg, `
			systemctl disable --now xrdp xrdp-sesman 2>/dev/null || true
			apt_purge_recorded
			if [ -f /etc/skel/.xsession ] && [ "$(cat /etc/skel/.xsession)" = "xfce4-session" ]; then
				rm -f /etc/skel/.xsession
			fi
		`)
		if err != nil {
			return err
		}
		if err := removeIfEmbeddedEqual("configs/45-allow-colord.rules", polkitRulesPath); err != nil {
			return err
		}
		return removeIfEmbeddedEqual("configs/45-allow-colord.pkla", polkitPklaPath)
	case "panel_launcher":
		return uninstallLauncher(cfg)

	case "browser":
		err := forEachModuleUser(mod, *cfg, func(u Config) error {
			return runBash(u, `
				HELPERS="$TARGET_HOME/.config/xfce4/helpers.rc"
				if [ -f "$HELPERS" ]; then
					as_user sed -i '/^WebBrowser=firefox$/d' "$HELPERS"
				fi
			`)
		})
		if err != nil {
			return err
		}
		return runBash(*cfg, `
			apt_purge_recorded
			if ! is_installed firefox; then
				rm -f /etc/apt/sources.list.d/mozilla.list /etc/apt/preferences.d/mozilla /etc/apt/keyrings/packages.mozilla.org.asc
			fi
		`, "KEEP_PKGS=ca-certificates\ncurl\ngnupg")

	case "ru_layout":
		err := forEachModuleUser(mod, *cfg, func(u Config) error {
			return runBash(u, `rm -f "$TARGET_HOME/.config/autostart/setxkbmap.desktop"`)
		})
		if err != nil {
			return err
		}
		return runBash(*cfg, `
			ORIG="$STATE_DIR/ru_layout.keyboard.orig"
			if [ -f "$ORIG" ]; then
				cp "$ORIG" /etc/default/keyboard
				rm -f "$ORIG"
			elif [ -f /etc/default/keyboard ]; then
				sed -i -e 's/^XKBLAYOUT=.*/XKBLAYOUT="us"/' -e 's/^XKBVARIANT=.*/XKBVARIANT=""/' -e 's/^XKBOPTIONS=.*/XKBOPTIONS=""/' /etc/default/keyboard
			fi
			apt_purge_recorded
		`)
	case "kitty_fonts":
		err := forEachModuleUser(mod, *cfg, func(u Config) error {
			if err := removeIfEmbeddedEqual("configs/kitty.conf", filepath.Join(u.TargetHome, ".config", "kitty", "kitty.conf")); err != nil {
				return err
			}
			return runBash(u, `
				fix_user_dirs
				rm -rf "$TARGET_HOME/.local/share/fonts/JetBrainsMono" "$TARGET_HOME/.local/share/fonts/FiraCode"
				rm -f "$TARGET_HOME/.local/share/xfce4/helpers/kitty.desktop"
				rmdir "$TARGET_HOME/.config/kitty" 2>/dev/null || true
				HELPERS="$TARGET_HOME/.config/xfce4/helpers.rc"
				if [ -f "$HELPERS" ]; then
					as_user sed -i '/^TerminalEmulator=kitty$/d' "$HELPERS"
				fi
				if command -v fc-cache >/dev/null 2>&1; then
					as_user fc-cache -f || true
				fi
			`)
		})
		if err != nil {
			return err
		}
		if err := unregisterPanelItems(*cfg, []panelItem{kittyPanelItem()}); err != nil {
			return err
		}
		return runBash(*cfg, "apt_purge_recorded", "KEEP_PKGS=unzip\ncurl\nfontconfig")
	case "zsh_starship":
		err := forEachModuleUser(mod, *cfg, func(u Config) error {
			return runBash(u, `
				fix_user_dirs
				PREV_FILE="$STATE_DIR/zsh_starship.prev-shell.$TARGET_USER"
				PREV=/bin/bash
				if [ -s "$PREV_FILE" ]; then
					PREV=$(cat "$PREV_FILE")
				fi
				case "$PREV" in
					*zsh) PREV=/bin/bash ;;
				esac
				if [ ! -x "$PREV" ]; then
					PREV=/bin/bash
				fi
				if [ "$(getent passwd "$TARGET_USER" | cut -d: -f7)" != "$PREV" ]; then
					chsh -s "$PREV" "$TARGET_USER"
				fi
				rm -f "$PREV_FILE"

				ZSHRC="$TARGET_HOME/.zshrc"
				if [ -f "$ZSHRC" ]; then
					as_user sed -i '/^# >>> vps-setup zsh >>>$/,/^# <<< vps-setup zsh <<<$/d' "$ZSHRC"
					if ! grep -q '[^[:space:]]' "$ZSHRC"; then
						rm -f "$ZSHRC"
					fi
				fi

				TOML_MARK="$STATE_DIR/zsh_starship.toml.$TARGET_USER"
				if [ -f "$TOML_MARK" ]; then
					rm -f "$TARGET_HOME/.config/starship.toml" "$TOML_MARK"
				fi
			`)
		})
		if err != nil {
			return err
		}
		return runBash(*cfg, `
			if [ -f "$STATE_DIR/zsh_starship.starship-installed" ]; then
				rm -f /usr/local/bin/starship "$STATE_DIR/zsh_starship.starship-installed"
			fi
			# zsh оставляем, если это всё ещё чья-то оболочка.
			KEEP_PKGS=curl
			if getent passwd | awk -F: '$7 ~ /zsh$/ {found=1} END {exit !found}'; then
				KEEP_PKGS=$(printf 'curl\nzsh\nzsh-common')
			fi
			apt_purge_recorded
		`)
	case "bbr":
		return runBash(*cfg, `
			rm -f /etc/sysctl.d/99-bbr.conf
			sysctl -w net.ipv4.tcp_congestion_control=cubic >/dev/null || true
			sysctl -w net.core.default_qdisc=fq_codel >/dev/null || true
			sysctl --system >/dev/null || true
		`)

	case "tailscale":
		return runBash(*cfg, `
			if command -v tailscale >/dev/null 2>&1; then
				tailscale down 2>/dev/null || true
			fi
			systemctl disable --now tailscaled 2>/dev/null || true
			apt_purge_recorded
			if ! is_installed tailscale; then
				rm -f /etc/apt/sources.list.d/tailscale.list /usr/share/keyrings/tailscale-archive-keyring.gpg
			fi
		`)

	case "ssh_hardening":
		return runBash(*cfg, sshHelpersFn+`
			BEFORE=$(sshd_ports)
			rm -f "$HARDENING_CONF" "$LEGACY_CONF"
			sshd -t
			# Сначала открываем порт, на который вернётся sshd, и только потом перезапускаем его.
			for p in $(sshd_ports); do
				case " $BEFORE " in
					*" $p "*) ;;
					*) open_ssh_port "$p" ;;
				esac
			done
			restart_sshd
		`, "HARDENING_CONF="+hardeningConf, "LEGACY_CONF="+legacyHardeningConf)

	case "fail2ban_ufw":
		return runBash(*cfg, `
			rm -f /etc/fail2ban/jail.local
			UFW_STATE="$STATE_DIR/fail2ban_ufw.ufw-state"
			if command -v ufw >/dev/null 2>&1 && [ "$(cat "$UFW_STATE" 2>/dev/null || echo disabled)" != enabled ]; then
				ufw --force disable || true
			fi
			rm -f "$UFW_STATE"
			apt_purge_recorded
			if is_installed fail2ban; then
				systemctl restart fail2ban || true
			fi
		`, "KEEP_PKGS=ufw")

	case "unattended":
		return runBash(*cfg, `
			cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "0";
EOF
			rm -f "$STATE_DIR/unattended.packages"
		`)

	case "ai_cli":
		err := forEachModuleUser(mod, *cfg, func(u Config) error {
			return runBash(u, `
				fix_user_dirs
				if as_user sh -c 'command -v uv >/dev/null 2>&1'; then
					as_user sh -c 'uv tool uninstall aider-chat' || true
				fi
				UV_MARK="$STATE_DIR/ai_cli.uv.$TARGET_USER"
				if [ -f "$UV_MARK" ]; then
					as_user sh -c 'rm -f "$HOME/.local/bin/uv" "$HOME/.local/bin/uvx"; rm -rf "$HOME/.local/share/uv"'
					rm -f "$UV_MARK"
				fi
			`)
		})
		if err != nil {
			return err
		}
		if err := unregisterPanelItems(*cfg, allAIToolItems()); err != nil {
			return err
		}
		return runBash(*cfg, `
			if command -v npm >/dev/null 2>&1; then
				npm uninstall -g @anthropic-ai/claude-code @openai/codex opencode-ai @google/gemini-cli || true
			fi
			apt_purge_recorded
			if ! is_installed nodejs; then
				rm -f /etc/apt/sources.list.d/nodesource.list /etc/apt/sources.list.d/nodesource.sources /usr/share/keyrings/nodesource.gpg /etc/apt/keyrings/nodesource.gpg
			fi
			if ! is_installed gh; then
				rm -f /etc/apt/sources.list.d/github-cli.list /etc/apt/keyrings/githubcli-archive-keyring.gpg
			fi
		`, "KEEP_PKGS=curl\nca-certificates\ngnupg")
	case "docker":
		if err := removeIfEmbeddedEqual("configs/daemon.json", "/etc/docker/daemon.json"); err != nil {
			return err
		}
		return runBash(*cfg, `
			systemctl disable --now docker docker.socket containerd 2>/dev/null || true
			apt_purge_recorded
			if ! is_installed docker-ce; then
				rm -f /etc/apt/sources.list.d/docker.list /etc/apt/keyrings/docker.asc
			fi
			rmdir /etc/docker 2>/dev/null || true
		`, "KEEP_PKGS=curl\nca-certificates\ngnupg")

	case "ollama":
		return runBash(*cfg, `
			systemctl disable --now ollama 2>/dev/null || true
			rm -f /etc/systemd/system/ollama.service
			systemctl daemon-reload || true
			OLLAMA_BIN=$(command -v ollama || true)
			if [ -n "$OLLAMA_BIN" ]; then
				rm -f "$OLLAMA_BIN"
			fi
			rm -rf /usr/local/lib/ollama /usr/share/ollama
			if id ollama >/dev/null 2>&1; then
				userdel ollama || true
			fi
			if getent group ollama >/dev/null 2>&1; then
				groupdel ollama || true
			fi
		`)
	}
	return fmt.Errorf("модуль %q удалить нельзя", mod)
}

// --- Пользователи, для которых ставились модули ---

// perUserModules меняют файлы в домашнем каталоге целевого пользователя. Для них
// запоминаем, для кого выполнялась установка, чтобы удаление почистило именно там
// (например, после create_user модули ставятся новому пользователю, а удаляет их другой).
var perUserModules = map[string]bool{"browser": true, "xrdp": true, "ru_layout": true, "kitty_fonts": true, "zsh_starship": true, "ai_cli": true}

func recordModuleUser(mod, username string) {
	if dryRun || !perUserModules[mod] || contains(moduleUsers(mod), username) {
		return
	}
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(stateDir, mod+".users"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(username + "\n")
}

func moduleUsers(mod string) []string {
	data, err := os.ReadFile(filepath.Join(stateDir, mod+".users"))
	if err != nil {
		return nil
	}
	var users []string
	for _, line := range strings.Split(string(data), "\n") {
		if name := strings.TrimSpace(line); name != "" && !contains(users, name) {
			users = append(users, name)
		}
	}
	return users
}

// forEachModuleUser выполняет пользовательскую часть удаления для всех, кому модуль
// ставился (если учёта нет — для текущего целевого пользователя), и чистит метки.
func forEachModuleUser(mod string, cfg Config, fn func(Config) error) error {
	users := moduleUsers(mod)
	if len(users) == 0 {
		users = []string{cfg.TargetUser}
	}
	for _, name := range users {
		if u, err := user.Lookup(name); err == nil {
			ucfg := cfg
			ucfg.TargetUser, ucfg.TargetHome = u.Username, u.HomeDir
			if err := fn(ucfg); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		removeUserMarkers(mod, name)
	}
	if !dryRun {
		_ = os.Remove(filepath.Join(stateDir, mod+".users"))
	}
	return nil
}

func removeUserMarkers(mod, username string) {
	if dryRun {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(stateDir, mod+".*."+username))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// markInstalled отмечает модуль как установленный программой — только такие модули
// предлагаются к удалению.
func markInstalled(mod string) {
	if dryRun {
		return
	}
	if err := os.MkdirAll(stateDir, 0755); err == nil {
		_ = os.WriteFile(filepath.Join(stateDir, mod+".installed"), []byte(time.Now().Format(time.RFC3339)+"\n"), 0644)
	}
}

// clearModuleState убирает весь учёт модуля после успешного удаления
// (create_user ведёт список пользователей сам).
func clearModuleState(mod string) {
	if dryRun {
		return
	}
	if mod == "create_user" {
		if len(existingCreatedUsers()) == 0 {
			_ = os.Remove(filepath.Join(stateDir, "create_user.installed"))
		}
		return
	}
	matches, _ := filepath.Glob(filepath.Join(stateDir, mod+".*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
