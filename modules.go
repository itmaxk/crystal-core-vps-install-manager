package main

import (
	"fmt"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// sshKeyRegex находит публичный ключ в строке authorized_keys, отбрасывая опции перед ним
// (например, command="echo 'Please login as the user ubuntu'..." в root-ключах облачных образов).
const sshKeyRegex = `(sk-)?(ssh-(rsa|dss|ed25519)|ecdsa-sha2-nistp(256|384|521))(@openssh\.com)? AAAA[A-Za-z0-9+/=]+`

var (
	usernameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	sshKeyRe   = regexp.MustCompile(sshKeyRegex)
)

// Модули быстрого старта: на «голом» сервере первым делом нужен рабочий стол и вход по RDP.
var rdpPresetModules = []string{"create_user", "swap", "xfce", "xrdp", "panel_launcher", "browser", "ru_layout"}

// Модули, которым не нужен apt.
var noAptModules = map[string]bool{"bbr": true, "swap": true, "ssh_hardening": true, "create_user": true, "panel_launcher": true}

type ModuleDef struct {
	ID          string
	Title       string
	Description string
	DefaultOn   bool
	Removable   bool
	IsReady     func(cfg Config) bool
}

// getModuleDefs возвращает модули в порядке выполнения: сначала пользователь,
// рабочий стол и RDP (чтобы как можно раньше можно было подключиться), затем всё остальное.
// Удаление идёт в обратном порядке.
func getModuleDefs() []ModuleDef {
	return []ModuleDef{
		{
			ID:          "create_user",
			Title:       "Создать sudo-пользователя",
			Description: "новый аккаунт с паролем для RDP/sudo и перенос SSH-ключей",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(cfg Config) bool { return cfg.TargetUser != "root" },
		},
		{
			ID:          "update",
			Title:       "Обновление пакетов + CLI-утилиты",
			Description: "apt upgrade, curl, wget, git, htop, btop, tmux, unzip, net-tools, gnupg",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return false },
		},
		{
			ID:          "swap",
			Title:       "Swap-файл 2 ГБ",
			Description: "файл подкачки в /etc/fstab для стабильной работы UI на малом объеме RAM",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(Config) bool { return swapActive("/swapfile") },
		},
		{
			ID:          "xfce",
			Title:       "Графическая оболочка XFCE",
			Description: "рабочий стол + удаление screensaver/light-locker (фикс черного экрана RDP)",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(Config) bool { return hasBin("xfce4-session") },
		},
		{
			ID:          "xrdp",
			Title:       "Сервер XRDP",
			Description: "удаленный рабочий стол на 127.0.0.1:3389 (или через Tailscale) + фикс polkit",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(Config) bool { return serviceActive("xrdp") },
		},
		{
			ID:          "panel_launcher",
			Title:       "Значок этой программы на панели XFCE",
			Description: "запуск vps-setup с нижней панели и из меню «Приложения → Система»",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(Config) bool { return fileExists(desktopFilePath) },
		},
		{
			ID:          "browser",
			Title:       "Браузер Firefox",
			Description: "deb-пакет из репозитория Mozilla (не snap) + русский язык; браузер по умолчанию",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(Config) bool { return fileExists("/usr/lib/firefox/firefox") },
		},
		{
			ID:          "ru_layout",
			Title:       "Русская раскладка",
			Description: "локаль ru_RU.UTF-8 и переключение US/RU по Alt+Shift в системе и XFCE",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(Config) bool { return fileContains("/etc/default/keyboard", `"us,ru"`) },
		},
		{
			ID:          "kitty_fonts",
			Title:       "Терминал Kitty + Nerd Fonts",
			Description: "быстрый терминал по умолчанию (значок на панели) и шрифты JetBrainsMono / FiraCode",
			DefaultOn:   true,
			Removable:   true,
			IsReady: func(cfg Config) bool {
				return hasBin("kitty") && fileExists(filepath.Join(cfg.TargetHome, ".local", "share", "fonts", "JetBrainsMono"))
			},
		},
		{
			ID:          "zsh_starship",
			Title:       "Zsh + Starship",
			Description: "удобная оболочка, красивый промпт, автодополнение и подсветка команд",
			DefaultOn:   true,
			Removable:   true,
			IsReady: func(cfg Config) bool {
				return hasBin("zsh") && hasBin("starship") && fileContains(filepath.Join(cfg.TargetHome, ".zshrc"), "# >>> vps-setup zsh >>>")
			},
		},
		{
			ID:          "bbr",
			Title:       "Сетевой алгоритм TCP BBR",
			Description: "ускорение сети в sysctl для плавного отклика интерфейса по RDP",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(Config) bool { return fileExists("/etc/sysctl.d/99-bbr.conf") },
		},
		{
			ID:          "tailscale",
			Title:       "Tailscale (mesh-VPN)",
			Description: "приватная шифрованная сеть для прямого входа по RDP без SSH-туннеля",
			DefaultOn:   false,
			Removable:   true,
			IsReady:     func(Config) bool { return hasBin("tailscale") && serviceActive("tailscaled") },
		},
		{
			ID:          "ssh_hardening",
			Title:       "Харденинг SSH",
			Description: "запрет входа под root и по паролю (только ключи), опционально смена порта",
			DefaultOn:   true,
			Removable:   true,
			IsReady: func(Config) bool {
				return fileExists(hardeningConf) || fileExists(legacyHardeningConf)
			},
		},
		{
			ID:          "fail2ban_ufw",
			Title:       "Fail2Ban + файрвол UFW",
			Description: "бан за перебор паролей и закрытие всех портов, кроме SSH (и RDP в Tailscale)",
			DefaultOn:   true,
			Removable:   true,
			IsReady:     func(Config) bool { return serviceActive("fail2ban") && fileExists("/etc/fail2ban/jail.local") },
		},
		{
			ID:          "unattended",
			Title:       "Автообновления безопасности",
			Description: "фоновая установка критических патчей безопасности Ubuntu",
			DefaultOn:   true,
			Removable:   true,
			IsReady: func(Config) bool {
				return hasBin("unattended-upgrade") && fileContains("/etc/apt/apt.conf.d/20auto-upgrades", `Unattended-Upgrade "1"`)
			},
		},
		{
			ID:          "ai_cli",
			Title:       "AI CLI-инструменты",
			Description: "Claude Code, Codex, OpenCode, Gemini, Aider (значки на панели) + Node.js 22, uv, gh",
			DefaultOn:   true,
			Removable:   true,
			IsReady: func(cfg Config) bool {
				aiderBin := filepath.Join(cfg.TargetHome, ".local", "bin", "aider")
				return hasBin("claude") && hasBin("codex") && hasBin("opencode") && hasBin("gh") && (hasBin("aider") || fileExists(aiderBin))
			},
		},
		{
			ID:          "docker",
			Title:       "Docker Engine + Compose v2",
			Description: "контейнеры, права для пользователя (группа docker) и ротация логов",
			DefaultOn:   false,
			Removable:   true,
			IsReady:     func(Config) bool { return hasBin("docker") && serviceActive("docker") },
		},
		{
			ID:          "ollama",
			Title:       "Ollama (локальные LLM)",
			Description: "запуск локальных ИИ-моделей на 127.0.0.1:11434 без отправки данных в облако",
			DefaultOn:   false,
			Removable:   true,
			IsReady:     func(Config) bool { return hasBin("ollama") && serviceActive("ollama") },
		},
		{
			ID:          "upgrade_ubuntu",
			Title:       "Переход на новую LTS-версию Ubuntu",
			Description: "full-upgrade + do-release-upgrade (долго, требует перезагрузки; выполняется последним)",
			DefaultOn:   false,
			IsReady:     func(Config) bool { return false },
		},
	}
}

func moduleDef(id string) (ModuleDef, bool) {
	for _, m := range getModuleDefs() {
		if m.ID == id {
			return m, true
		}
	}
	return ModuleDef{}, false
}

func moduleTitle(id string) string {
	if m, ok := moduleDef(id); ok {
		return m.Title
	}
	return id
}

// presetModules возвращает ещё не установленные модули набора.
func presetModules(preset string, cfg Config) ([]string, error) {
	var result []string
	for _, m := range getModuleDefs() {
		var inPreset bool
		switch preset {
		case presetRDP:
			inPreset = contains(rdpPresetModules, m.ID)
		case presetFull:
			inPreset = m.DefaultOn
		default:
			return nil, fmt.Errorf("неизвестный набор --preset %q (доступно: %s, %s)", preset, presetRDP, presetFull)
		}
		if inPreset && !m.IsReady(cfg) {
			result = append(result, m.ID)
		}
	}
	return result, nil
}

func parseOnlyModules(raw string) ([]string, error) {
	var result []string
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, ok := moduleDef(id); !ok {
			return nil, fmt.Errorf("неизвестный модуль: %q (доступные ID смотрите через --status)", id)
		}
		result = append(result, id)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("список модулей пуст")
	}
	return orderModules(result), nil
}

// orderModules убирает дубликаты и выстраивает модули в порядке getModuleDefs —
// независимо от того, в каком порядке их перечислили или отметили.
func orderModules(mods []string) []string {
	index := make(map[string]int)
	for i, m := range getModuleDefs() {
		index[m.ID] = i
	}
	seen := make(map[string]bool)
	var result []string
	for _, m := range mods {
		if !seen[m] {
			seen[m] = true
			result = append(result, m)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, okA := index[result[i]]
		b, okB := index[result[j]]
		if !okA {
			a = len(index)
		}
		if !okB {
			b = len(index)
		}
		return a < b
	})
	return result
}

func needsApt(mods []string) bool {
	for _, m := range mods {
		if !noAptModules[m] {
			return true
		}
	}
	return false
}

// --- Установка модулей ---
//
// Всё, что модуль меняет, записывается в $STATE_DIR (/var/lib/vps-setup), чтобы удаление
// вернуло систему в исходное состояние: apt_install запоминает только пакеты, которых
// до установки не было, рядом лежат резервные копии изменённых системных файлов.

func executeModule(mod string, cfg *Config) error {
	currentModule = mod
	switch mod {
	case "create_user":
		return moduleCreateUser(cfg)

	case "update":
		return runBash(*cfg, `
			apt-get upgrade
			apt_install curl wget git htop btop tmux unzip fontconfig software-properties-common net-tools ca-certificates gnupg
		`)

	case "swap":
		return runBash(*cfg, `
			if ! swapon --show=NAME --noheadings | grep -qx /swapfile; then
				if [ ! -f /swapfile ]; then
					fallocate -l 2G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=2048 status=none
				fi
				chmod 600 /swapfile
				mkswap /swapfile
				swapon /swapfile
			fi
			grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
		`)

	case "xfce":
		return runBash(*cfg, `
			apt_install xfce4 xfce4-goodies dbus-x11 x11-xserver-utils
			apt-get purge xfce4-screensaver light-locker || true
			apt-get autoremove

			# У VPS нет монитора: локальный дисплей-менеджер не нужен и только съедает RAM.
			if [ ! -f "$STATE_DIR/xfce.default-target" ]; then
				systemctl get-default > "$STATE_DIR/xfce.default-target" 2>/dev/null || true
			fi
			systemctl set-default multi-user.target
			for dm in lightdm gdm3 sddm; do
				if systemctl is-enabled --quiet "$dm" 2>/dev/null; then
					systemctl disable --now "$dm" || true
				fi
			done
		`)

	case "xrdp":
		return moduleXRDP(cfg)

	case "panel_launcher":
		return moduleLauncher(cfg)

	case "browser":
		return runBash(*cfg, `
			fix_user_dirs
			apt_install ca-certificates curl gnupg
			install -m 0755 -d /etc/apt/keyrings
			if [ ! -f /etc/apt/keyrings/packages.mozilla.org.asc ]; then
				curl -fsSL https://packages.mozilla.org/apt/repo-signing-key.gpg -o /etc/apt/keyrings/packages.mozilla.org.asc
				chmod a+r /etc/apt/keyrings/packages.mozilla.org.asc
			fi
			echo "deb [signed-by=/etc/apt/keyrings/packages.mozilla.org.asc] https://packages.mozilla.org/apt mozilla main" > /etc/apt/sources.list.d/mozilla.list
			# Приоритет 1000: deb от Mozilla вместо snap-заглушки Ubuntu (snap плохо работает в RDP-сессиях).
			cat > /etc/apt/preferences.d/mozilla <<'EOF'
Package: *
Pin: origin packages.mozilla.org
Pin-Priority: 1000
EOF
			apt-get update
			apt_install firefox firefox-l10n-ru

			# Браузер по умолчанию в XFCE (значок «Веб-браузер» на нижней панели).
			as_user mkdir -p "$TARGET_HOME/.config/xfce4"
			HELPERS="$TARGET_HOME/.config/xfce4/helpers.rc"
			if [ -f "$HELPERS" ] && grep -q '^WebBrowser=' "$HELPERS"; then
				as_user sed -i 's/^WebBrowser=.*/WebBrowser=firefox/' "$HELPERS"
			else
				echo "WebBrowser=firefox" | as_user tee -a "$HELPERS" >/dev/null
			fi
		`)

	case "ru_layout":
		return runBash(*cfg, `
			fix_user_dirs
			apt_install locales keyboard-configuration
			locale-gen en_US.UTF-8 ru_RU.UTF-8

			if [ ! -f "$STATE_DIR/ru_layout.keyboard.orig" ] && [ -f /etc/default/keyboard ] && ! grep -q '"us,ru"' /etc/default/keyboard; then
				cp /etc/default/keyboard "$STATE_DIR/ru_layout.keyboard.orig"
			fi
			cat > /etc/default/keyboard <<'EOF'
XKBMODEL="pc105"
XKBLAYOUT="us,ru"
XKBVARIANT=","
XKBOPTIONS="grp:alt_shift_toggle"
BACKSPACE="guess"
EOF

			as_user mkdir -p "$TARGET_HOME/.config/autostart"
			as_user tee "$TARGET_HOME/.config/autostart/setxkbmap.desktop" >/dev/null <<'EOF'
[Desktop Entry]
Type=Application
Name=Keyboard Layout US/RU
Exec=setxkbmap -layout us,ru -option grp:alt_shift_toggle
Hidden=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
EOF
		`)

	case "kitty_fonts":
		kittyConf := filepath.Join(cfg.TargetHome, ".config", "kitty", "kitty.conf")
		if err := writeUserFile(*cfg, "configs/kitty.conf", kittyConf, 0644, false); err != nil {
			return err
		}
		err := runBash(*cfg, `
			fix_user_dirs
			apt_install kitty unzip curl fontconfig

			# Kitty — терминал по умолчанию в XFCE (нужен helper-файл, kitty его не поставляет).
			as_user mkdir -p "$TARGET_HOME/.config/xfce4" "$TARGET_HOME/.local/share/xfce4/helpers" "$TARGET_HOME/.local/share/fonts"
			as_user tee "$TARGET_HOME/.local/share/xfce4/helpers/kitty.desktop" >/dev/null <<'EOF'
[Desktop Entry]
NoDisplay=true
Version=1.0
Encoding=UTF-8
Type=X-XFCE-Helper
X-XFCE-Category=TerminalEmulator
X-XFCE-Commands=kitty
X-XFCE-CommandsWithParameter=kitty %s
Icon=kitty
Name=kitty
EOF
			HELPERS="$TARGET_HOME/.config/xfce4/helpers.rc"
			if [ -f "$HELPERS" ] && grep -q '^TerminalEmulator=' "$HELPERS"; then
				as_user sed -i 's/^TerminalEmulator=.*/TerminalEmulator=kitty/' "$HELPERS"
			else
				echo "TerminalEmulator=kitty" | as_user tee -a "$HELPERS" >/dev/null
			fi

			FONT_DIR="$TARGET_HOME/.local/share/fonts"
			TMP=$(mktemp -d)
			trap 'rm -rf "$TMP"' EXIT
			for FONT in JetBrainsMono FiraCode; do
				if [ ! -d "$FONT_DIR/$FONT" ]; then
					curl -fsSL --retry 3 "https://github.com/ryanoasis/nerd-fonts/releases/latest/download/${FONT}.zip" -o "$TMP/${FONT}.zip"
					mkdir -p "$TMP/$FONT"
					unzip -oq "$TMP/${FONT}.zip" -d "$TMP/$FONT"
					chown -R "$TARGET_USER:" "$TMP/$FONT"
					mv "$TMP/$FONT" "$FONT_DIR/$FONT"
				fi
			done
			as_user fc-cache -f
		`)
		if err != nil {
			return err
		}
		return registerPanelItems(*cfg, []panelItem{kittyPanelItem()})

	case "zsh_starship":
		zshrcPath := filepath.Join(cfg.TargetHome, ".zshrc")
		if err := appendEmbeddedBlock("configs/zshrc.snippet", zshrcPath, "# >>> vps-setup zsh >>>"); err != nil {
			return err
		}
		if err := chownToUser(*cfg, zshrcPath); err != nil {
			return err
		}
		return runBash(*cfg, `
			fix_user_dirs
			apt_install zsh zsh-autosuggestions zsh-syntax-highlighting curl

			if ! command -v starship >/dev/null 2>&1; then
				curl -fsSL https://starship.rs/install.sh | sh -s -- -y
				touch "$STATE_DIR/zsh_starship.starship-installed"
			fi

			PREV_SHELL_FILE="$STATE_DIR/zsh_starship.prev-shell.$TARGET_USER"
			if [ ! -f "$PREV_SHELL_FILE" ]; then
				getent passwd "$TARGET_USER" | cut -d: -f7 > "$PREV_SHELL_FILE"
			fi
			chsh -s "$(command -v zsh)" "$TARGET_USER"

			as_user mkdir -p "$TARGET_HOME/.config"
			if [ ! -f "$TARGET_HOME/.config/starship.toml" ]; then
				as_user starship preset nerd-font-symbols -o "$TARGET_HOME/.config/starship.toml"
				touch "$STATE_DIR/zsh_starship.toml.$TARGET_USER"
			fi
		`)

	case "bbr":
		return runBash(*cfg, `
			modprobe tcp_bbr 2>/dev/null || true
			cat > /etc/sysctl.d/99-bbr.conf <<'EOF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
EOF
			sysctl --system >/dev/null
		`)

	case "tailscale":
		return runBash(*cfg, `
			if ! command -v tailscale >/dev/null 2>&1; then
				curl -fsSL https://tailscale.com/install.sh | sh
				echo tailscale >> "$STATE_DIR/tailscale.packages"
			fi
			systemctl enable --now tailscaled
		`)

	case "ssh_hardening":
		return moduleSSHHardening(cfg)

	case "fail2ban_ufw":
		return moduleFail2banUFW(cfg)

	case "unattended":
		return runBash(*cfg, `
			apt_install unattended-upgrades
			cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
			systemctl enable --now unattended-upgrades || true
		`)

	case "ai_cli":
		err := runBash(*cfg, `
			apt_install curl ca-certificates gnupg ripgrep fd-find jq

			# GitHub CLI из официального репозитория (в universe лежит сильно устаревшая версия).
			if ! command -v gh >/dev/null 2>&1; then
				install -m 0755 -d /etc/apt/keyrings
				curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
				chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
				echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list
				apt-get update
				apt_install gh
			fi

			if ! command -v node >/dev/null 2>&1 || [ "$(node -v | cut -d. -f1 | tr -d 'v')" -lt 20 ]; then
				curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
				apt_install nodejs
			fi

			npm install -g \
				@anthropic-ai/claude-code \
				@openai/codex \
				opencode-ai \
				@google/gemini-cli

			fix_user_dirs
			if ! as_user sh -c 'command -v uv >/dev/null 2>&1'; then
				touch "$STATE_DIR/ai_cli.uv.$TARGET_USER"
			fi
			as_user sh -c '
				if ! command -v uv >/dev/null 2>&1; then
					curl -LsSf https://astral.sh/uv/install.sh | sh
				fi
				uv tool install --python 3.12 --upgrade aider-chat
			'
		`)
		if err != nil {
			return err
		}
		return registerPanelItems(*cfg, installedAIToolItems(*cfg))

	case "docker":
		if !fileExists("/etc/docker/daemon.json") {
			if err := writeEmbeddedFile("configs/daemon.json", "/etc/docker/daemon.json", 0644); err != nil {
				return err
			}
		}
		return runBash(*cfg, `
			apt_install ca-certificates curl gnupg

			install -m 0755 -d /etc/apt/keyrings
			if [ ! -f /etc/apt/keyrings/docker.asc ]; then
				curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
				chmod a+r /etc/apt/keyrings/docker.asc
			fi

			CODENAME=$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
			echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $CODENAME stable" > /etc/apt/sources.list.d/docker.list

			apt-get update
			apt_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

			usermod -aG docker "$TARGET_USER"
			systemctl enable docker
			systemctl restart docker
		`)

	case "ollama":
		return runBash(*cfg, `
			if ! command -v ollama >/dev/null 2>&1; then
				curl -fsSL https://ollama.com/install.sh | sh
			fi
			systemctl enable ollama
			systemctl restart ollama
		`)

	case "upgrade_ubuntu":
		return runBash(*cfg, `
			apt-get full-upgrade
			apt-get install update-manager-core
			apt-get autoremove

			if [ -f /etc/update-manager/release-upgrades ]; then
				sed -i 's/^Prompt=.*/Prompt=lts/' /etc/update-manager/release-upgrades
			fi

			# do-release-upgrade откажется работать, пока не выполнена перезагрузка после обновления ядра.
			if [ -f /var/run/reboot-required ]; then
				echo "Требуется перезагрузка — переход на новую LTS отложен до следующего запуска."
				exit 0
			fi

			if do-release-upgrade -c; then
				do-release-upgrade -f DistUpgradeViewNonInteractive
			fi
		`)
	}
	return fmt.Errorf("неизвестный модуль %q", mod)
}

// extractKeysFn — bash-функция, которая достаёт из authorized_keys только сами ключи,
// без опций вида command="..." (иначе новый пользователь получит ключ-«заглушку» root).
const extractKeysFn = `
extract_keys() {
	grep -ohE "${KEY_RE}( .*)?$" "$@" || true
}
`

func moduleCreateUser(cfg *Config) error {
	username := strings.TrimSpace(cfg.NewUsername)
	if err := validateUsername(username); err != nil {
		return err
	}

	err := runBash(*cfg, extractKeysFn+`
		if ! id -u "$NEW_USER" >/dev/null 2>&1; then
			useradd -m -U -s /bin/bash "$NEW_USER"
			echo "$NEW_USER" >> "$STATE_DIR/create_user.users"
		fi
		usermod -aG sudo,users "$NEW_USER"

		USER_HOME=$(getent passwd "$NEW_USER" | cut -d: -f6)
		install -d -m 700 -o "$NEW_USER" -g "$(id -gn "$NEW_USER")" "$USER_HOME/.ssh"
		AK="$USER_HOME/.ssh/authorized_keys"
		touch "$AK"

		SOURCES=""
		for src in /root/.ssh/authorized_keys "${SOURCE_HOME:+$SOURCE_HOME/.ssh/authorized_keys}"; do
			if [ -n "$src" ] && [ -f "$src" ]; then
				SOURCES="$SOURCES $src"
			fi
		done
		if [ -n "$SOURCES" ]; then
			# shellcheck disable=SC2086
			extract_keys $SOURCES >> "$AK"
		fi
		sort -u -o "$AK" "$AK"
		chmod 600 "$AK"
		chown "$NEW_USER:" "$AK"
	`, "NEW_USER="+username, "SOURCE_HOME="+cfg.SourceHome, "KEY_RE="+sshKeyRegex)
	if err != nil {
		return err
	}

	if dryRun {
		fmt.Println(dryStyle.Render(fmt.Sprintf("  [DRY-RUN] Установка пароля через chpasswd для %s", username)))
		cfg.TargetUser = username
		cfg.TargetHome = filepath.Join("/home", username)
		return nil
	}

	if err := setPassword(username, cfg.Password); err != nil {
		return err
	}

	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("не удалось прочитать профиль %s: %w", username, err)
	}
	cfg.TargetUser = u.Username
	cfg.TargetHome = u.HomeDir
	return nil
}

// xrdpIniEdit меняет порт только в секции [Globals]. Прежний вариант (`s|^port=.*|...|g`)
// заодно перезаписывал `port=-1` в секциях [Xorg]/[Xvnc], и сессия не запускалась.
const xrdpIniEdit = `
sed -i -E "0,/^port=/s|^port=.*|port=${XRDP_PORT}|" "$XRDP_INI"
sed -i -E '/^\[(Xorg|Xvnc)\]/,/^\[/ s|^port=.*|port=-1|' "$XRDP_INI"
`

const (
	polkitRulesPath = "/etc/polkit-1/rules.d/45-allow-colord.rules"
	polkitPklaPath  = "/etc/polkit-1/localauthority/50-local.d/45-allow-colord.pkla"
)

func moduleXRDP(cfg *Config) error {
	xrdpPort := "tcp://127.0.0.1:3389"
	if contains(cfg.Modules, "tailscale") {
		xrdpPort = "3389"
	}

	if err := writeEmbeddedFile("configs/45-allow-colord.rules", polkitRulesPath, 0644); err != nil {
		return err
	}
	if err := writeEmbeddedFile("configs/45-allow-colord.pkla", polkitPklaPath, 0644); err != nil {
		return err
	}

	err := runBash(*cfg, `
		fix_user_dirs
		apt_install xrdp xorgxrdp
		usermod -aG ssl-cert xrdp || true

		XRDP_INI=/etc/xrdp/xrdp.ini
		`+xrdpIniEdit+`

		echo "xfce4-session" > /etc/skel/.xsession
		echo "xfce4-session" | as_user tee "$TARGET_HOME/.xsession" >/dev/null

		STARTWM=/etc/xrdp/startwm.sh
		if ! grep -q "unset DBUS_SESSION_BUS_ADDRESS" "$STARTWM"; then
			sed -i '/test -x \/etc\/X11\/Xsession/i unset DBUS_SESSION_BUS_ADDRESS\nunset XDG_RUNTIME_DIR' "$STARTWM"
		fi
		if ! grep -q "unset DBUS_SESSION_BUS_ADDRESS" "$STARTWM"; then
			sed -i '1a unset DBUS_SESSION_BUS_ADDRESS\nunset XDG_RUNTIME_DIR' "$STARTWM"
		fi

		systemctl enable xrdp
		systemctl restart xrdp
	`, "XRDP_PORT="+xrdpPort)
	if err != nil {
		return err
	}

	// RDP-вход возможен только по паролю: у пользователей облачных образов (ubuntu, opc) его нет.
	if cfg.Password != "" && cfg.TargetUser != "root" && !contains(cfg.Modules, "create_user") {
		if dryRun {
			fmt.Println(dryStyle.Render(fmt.Sprintf("  [DRY-RUN] Установка пароля для RDP через chpasswd для %s", cfg.TargetUser)))
			return nil
		}
		if !userHasPassword(cfg.TargetUser) {
			return setPassword(cfg.TargetUser, cfg.Password)
		}
	}
	return nil
}

// sshHelpersFn — общие функции для установки и удаления ssh_hardening.
//   - open_ssh_port открывает порт в UFW (если включён) и в iptables, если там есть
//     REJECT/DROP (как в образах Oracle Cloud), иначе новые подключения не пройдут;
//   - restart_sshd учитывает активацию через ssh.socket (Ubuntu 22.10+).
const sshHelpersFn = `
mkdir -p /etc/ssh/sshd_config.d /run/sshd
sshd_ports() {
	sshd -T 2>/dev/null | awk '$1=="port"{print $2}' | tr '\n' ' ' || true
}
open_ssh_port() {
	local port=$1
	if command -v ufw >/dev/null 2>&1 && grep -q "Status: active" <<<"$(ufw status 2>/dev/null || true)"; then
		ufw allow "$port/tcp" comment 'SSH'
	fi
	if command -v iptables >/dev/null 2>&1 && grep -qE -- '-j (REJECT|DROP)|^-P INPUT DROP' <<<"$(iptables -S INPUT 2>/dev/null || true)"; then
		iptables -C INPUT -p tcp --dport "$port" -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p tcp --dport "$port" -j ACCEPT
		if command -v netfilter-persistent >/dev/null 2>&1; then netfilter-persistent save; fi
	fi
}
restart_sshd() {
	if systemctl is-enabled --quiet ssh.socket 2>/dev/null || systemctl is-active --quiet ssh.socket 2>/dev/null; then
		systemctl daemon-reload
		systemctl restart ssh.socket
		if systemctl is-active --quiet ssh.service; then
			systemctl restart ssh.service
		fi
	else
		systemctl restart ssh 2>/dev/null || systemctl restart sshd
	fi
}
`

func moduleSSHHardening(cfg *Config) error {
	if cfg.TargetUser == "root" {
		return fmt.Errorf("целевой пользователь — root, а модуль запрещает вход под root; сначала создайте пользователя (create_user)")
	}
	authKeys := filepath.Join(cfg.TargetHome, ".ssh", "authorized_keys")
	if !hasValidKeyFile(authKeys) {
		if !dryRun {
			return fmt.Errorf("в %s нет ни одного SSH-ключа — добавьте ключ перед отключением паролей", authKeys)
		}
		fmt.Println(warnStyle.Render(fmt.Sprintf("  [DRY-RUN] Внимание: в %s нет SSH-ключей (в реальном запуске модуль остановится)", authKeys)))
	}

	return runBash(*cfg, sshHelpersFn+`
		CURRENT_PORTS=$(sshd_ports)
		case " $CURRENT_PORTS " in
			*" $SSH_PORT "*) ;;
			*) open_ssh_port "$SSH_PORT" ;;
		esac

		# Имя 01-* важно: sshd берёт ПЕРВОЕ значение параметра, а 50-cloud-init.conf
		# в облачных образах включает PasswordAuthentication yes.
		rm -f "$LEGACY_CONF"
		cat > "$HARDENING_CONF" <<EOF
Port $SSH_PORT
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
EOF
		if ! sshd -t; then
			rm -f "$HARDENING_CONF"
			echo "sshd -t: конфигурация некорректна, изменения откатаны" >&2
			exit 1
		fi

		EFFECTIVE=$(sshd -T)
		for want in "port $SSH_PORT" "permitrootlogin no" "passwordauthentication no"; do
			if ! grep -qx "$want" <<<"$EFFECTIVE"; then
				rm -f "$HARDENING_CONF"
				echo "sshd не применил '$want' (проверьте Include в /etc/ssh/sshd_config), изменения откатаны" >&2
				exit 1
			fi
		done

		restart_sshd
	`, "SSH_PORT="+cfg.SSHPort, "HARDENING_CONF="+hardeningConf, "LEGACY_CONF="+legacyHardeningConf)
}

func moduleFail2banUFW(cfg *Config) error {
	if err := runBash(*cfg, `
		if command -v ufw >/dev/null 2>&1 && [ ! -f "$STATE_DIR/fail2ban_ufw.ufw-state" ]; then
			if grep -q "Status: active" <<<"$(ufw status 2>/dev/null || true)"; then
				echo enabled > "$STATE_DIR/fail2ban_ufw.ufw-state"
			else
				echo disabled > "$STATE_DIR/fail2ban_ufw.ufw-state"
			fi
		fi
		apt_install fail2ban ufw python3-systemd
	`); err != nil {
		return err
	}

	// Открываем порты, на которых sshd реально слушает, — а не только --ssh-port.
	// Иначе при пропущенном ssh_hardening (sshd на 22, а в UFW 2222) доступ к серверу терялся.
	ports := detectSSHPorts()
	if contains(cfg.Modules, "ssh_hardening") && !contains(ports, cfg.SSHPort) {
		ports = append(ports, cfg.SSHPort)
	}

	jailCfg := *cfg
	jailCfg.SSHPort = strings.Join(ports, ",")
	if err := writeEmbeddedTemplate("configs/jail.local.tmpl", "/etc/fail2ban/jail.local", jailCfg, 0644); err != nil {
		return err
	}

	allowTailscale := "0"
	if contains(cfg.Modules, "tailscale") || hasBin("tailscale") {
		allowTailscale = "1"
	}

	return runBash(*cfg, `
		systemctl enable fail2ban
		systemctl restart fail2ban

		ufw default deny incoming
		ufw default allow outgoing
		for p in $SSH_PORTS; do
			ufw allow "$p/tcp" comment 'SSH'
		done
		if [ "$ALLOW_TAILSCALE_RDP" = 1 ]; then
			ufw allow in on tailscale0 to any port 3389 proto tcp comment 'RDP via Tailscale'
		fi
		ufw --force enable
	`, "SSH_PORTS="+strings.Join(ports, " "), "ALLOW_TAILSCALE_RDP="+allowTailscale)
}
