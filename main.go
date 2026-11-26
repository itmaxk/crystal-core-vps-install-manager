package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
)

//go:embed configs/*
var configFS embed.FS

var (
	Version = "dev"
	dryRun  bool

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4")).MarginBottom(1)
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")).Bold(true)
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87")).Bold(true)
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C"))
	dryStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD"))
)

const (
	logFile    = "/var/log/vps-setup.log"
	githubRepo = "your-username/vps-setup" // Замените на ваш репозиторий GitHub
)

type Config struct {
	TargetUser  string
	TargetHome  string
	SSHPort     string
	NewUsername string
	NewPassword string
	Modules     []string
}

type ModuleDef struct {
	ID          string
	Title       string
	Description string
	DefaultOn   bool
	IsReady     func(cfg Config) bool
}

type ModuleStatus struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Ready   bool   `json:"ready"`
	Details string `json:"details,omitempty"`
}

func main() {
	showVersion := flag.Bool("version", false, "Показать текущую версию")
	doUpdate := flag.Bool("self-update", false, "Обновить бинарник из GitHub Releases")
	showStatus := flag.Bool("status", false, "Показать состояние модулей")
	asJSON := flag.Bool("json", false, "Вывести статус в формате JSON (используется с --status)")
	onlyFlag := flag.String("only", "", "Неинтерактивный запуск указанных модулей через запятую (например: xfce,xrdp,docker)")
	userFlag := flag.String("user", "", "Целевой пользователь (для запуска под root из cloud-init / Ansible)")
	sshPortFlag := flag.String("ssh-port", "2222", "Порт SSH для неинтерактивного режима (по умолчанию 2222)")
	autoReboot := flag.Bool("auto-reboot", false, "Автоматически перезагрузить сервер после upgrade_ubuntu, если требуется")
	flag.BoolVar(&dryRun, "dry-run", false, "Показать команды и файлы без внесения изменений в систему")
	flag.Parse()

	if !(*showStatus && *asJSON) {
		printBanner()
	}

	if *showVersion {
		fmt.Printf("Crystal-Core VPS Install Manager %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return
	}

	if *doUpdate {
		if err := selfUpdate(); err != nil {
			fmt.Println(errStyle.Render("✖ Ошибка обновления: " + err.Error()))
			os.Exit(1)
		}
		return
	}

	if *showStatus {
		cfg, err := resolveUserConfig(*userFlag)
		if err != nil {
			fmt.Println(errStyle.Render("✖ " + err.Error()))
			os.Exit(1)
		}
		if *asJSON {
			printStatusJSON(cfg)
		} else {
			printStatusTable(cfg)
		}
		return
	}

	if !dryRun && os.Geteuid() != 0 {
		fmt.Println(errStyle.Render("Ошибка: запустите установщик через sudo (sudo ./vps-setup)"))
		os.Exit(1)
	}

	nonInteractive := *onlyFlag != ""
	cfg, err := resolveUserConfig(*userFlag)
	if err != nil {
		fmt.Println(errStyle.Render("✖ " + err.Error()))
		os.Exit(1)
	}
	cfg.SSHPort = *sshPortFlag

	if nonInteractive {
		mods, err := parseOnlyModules(*onlyFlag)
		if err != nil {
			fmt.Println(errStyle.Render("✖ " + err.Error()))
			os.Exit(1)
		}
		if p, err := strconv.Atoi(cfg.SSHPort); err != nil || p < 1 || p > 65535 {
			fmt.Println(errStyle.Render("✖ Некорректный --ssh-port (ожидается 1–65535)"))
			os.Exit(1)
		}
		cfg.Modules = mods
	} else {
		options, preselected := buildMenu(cfg)
		cfg.Modules = preselected

		form := huh.NewForm(
			huh.NewGroup(
				huh.NewMultiSelect[string]().
					Title("Выберите модули для установки / обновления").
					Description("Пробел — отметить/снять, ↑/↓ — навигация, Enter — запуск").
					Height(15).
					Options(options...).
					Value(&cfg.Modules),
			),
			huh.NewGroup(
				huh.NewInput().
					Title("Имя нового пользователя").
					Description("Строчные латинские буквы и цифры (например: dev, admin)").
					Value(&cfg.NewUsername).
					Validate(func(s string) error {
						s = strings.TrimSpace(s)
						if s == "" || s == "root" {
							return fmt.Errorf("укажите корректное имя пользователя (не root)")
						}
						return nil
					}),
				huh.NewInput().
					Title("Пароль пользователя (для sudo и входа по XRDP)").
					Description("Вход по SSH останется по ключам, пароль нужен для sudo и RDP").
					EchoMode(huh.EchoModePassword).
					Value(&cfg.NewPassword).
					Validate(func(s string) error {
						if len(s) < 6 {
							return fmt.Errorf("пароль должен быть не короче 6 символов")
						}
						return nil
					}),
			).WithHideFunc(func() bool {
				return !contains(cfg.Modules, "create_user")
			}),
			huh.NewGroup(
				huh.NewInput().
					Title("Порт для SSH").
					Description("Используется для харденинга SSH, правил UFW и Fail2Ban (1024–65535)").
					Value(&cfg.SSHPort).
					Validate(func(s string) error {
						p, err := strconv.Atoi(s)
						if err != nil || p < 1 || p > 65535 {
							return fmt.Errorf("укажите корректный порт (1–65535)")
						}
						return nil
					}),
			).WithHideFunc(func() bool {
				return !contains(cfg.Modules, "ssh_hardening") && !contains(cfg.Modules, "fail2ban_ufw")
			}),
		)

		if err := form.Run(); err != nil {
			fmt.Println(warnStyle.Render("Установка отменена."))
			os.Exit(0)
		}
	}

	if contains(cfg.Modules, "xrdp") && contains(cfg.Modules, "tailscale") && !contains(cfg.Modules, "fail2ban_ufw") {
		fmt.Println(warnStyle.Render("⚠ Внимание: выбран Tailscale + XRDP без UFW. Убедитесь, что порт 3389 закрыт внешним файрволом."))
	}

	if !dryRun {
		_ = os.WriteFile(logFile, []byte("=== VPS Setup Log ===\n"), 0644)
	}

	for _, mod := range cfg.Modules {
		var runErr error

		if nonInteractive || dryRun {
			fmt.Printf("--> Модуль [%s]\n", mod)
			runErr = executeModule(mod, &cfg)
		} else {
			_ = spinner.New().
				Title(fmt.Sprintf("Выполняется модуль: %s...", mod)).
				Action(func() {
					runErr = executeModule(mod, &cfg)
				}).
				Run()
		}

		if runErr != nil {
			fmt.Println(errStyle.Render(fmt.Sprintf("✖ Ошибка в модуле [%s]: %v", mod, runErr)))
			if !dryRun {
				fmt.Println(warnStyle.Render(fmt.Sprintf("Подробности смотрите в %s", logFile)))
			}
			os.Exit(1)
		}
		fmt.Println(okStyle.Render(fmt.Sprintf("✓ Модуль [%s] успешно применен", mod)))

		if !dryRun && mod == "upgrade_ubuntu" && fileExists("/var/run/reboot-required") {
			if *autoReboot {
				fmt.Println("\n" + warnStyle.Render("⚠ Требуется перезагрузка. Сервер перезагрузится через 5 секунд..."))
				if err := exec.Command("systemd-run", "--on-active=5s", "--quiet", "systemctl", "reboot").Run(); err != nil {
					_ = exec.Command("systemctl", "reboot").Run()
				}
				os.Exit(0)
			}

			fmt.Println("\n" + warnStyle.Render("⚠ Система обновила ядро или версию Ubuntu — требуется перезагрузка."))
			fmt.Println("Выполните `sudo reboot`, а затем запустите `./vps-setup` снова для установки остальных модулей.")
			os.Exit(0)
		}
	}

	fmt.Println("\n" + okStyle.Render("Готово! Все выбранные модули обработаны."))
	fmt.Printf("Туннель для RDP:\n  ssh -p %s -N -L 127.0.0.1:13389:localhost:3389 %s@<IP_СЕРВЕРА>\n", cfg.SSHPort, cfg.TargetUser)
	if contains(cfg.Modules, "tailscale") {
		fmt.Println(warnStyle.Render("Для активации Tailscale выполните: sudo tailscale up"))
	}
}

func printBanner() {
	crystalStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#00D1B2")).
		Bold(true).
		PaddingRight(2)

	crystalIcon := crystalStyle.Render(strings.Join([]string{
		"   /\\   ",
		"  /◈◈\\  ",
		" /◈\\/◈\\ ",
		" \\◈/\\◈/ ",
		"  \\◈◈/  ",
		"   \\/   ",
	}, "\n"))

	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4")).
		Render("Crystal-Core VPS Install Manager")
	subtitle := lipgloss.NewStyle().Foreground(lipgloss.Color("#00D1B2")).
		Render("Автоматизация бизнеса и IT-инфраструктура под ключ")
	desc := lipgloss.NewStyle().Foreground(lipgloss.Color("#CCCCCC")).
		Render("Заказная разработка, AI-агенты, интеграции и настройка серверов.")
	links := lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).
		Render("Сайт: https://crystal-core.com/  |  Почта: info@crystal-core.com")

	textBlock := lipgloss.JoinVertical(lipgloss.Left, title, subtitle, "", desc, links)

	bannerBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Padding(0, 2).
		MarginBottom(1)

	fmt.Println(bannerBox.Render(lipgloss.JoinHorizontal(lipgloss.Center, crystalIcon, textBlock)))
}

func resolveUserConfig(explicitUser string) (Config, error) {
	var u *user.User
	var err error

	switch {
	case explicitUser != "":
		u, err = user.Lookup(explicitUser)
		if err != nil {
			return Config{}, fmt.Errorf("пользователь %q не найден в системе: %w", explicitUser, err)
		}
	case os.Getenv("SUDO_USER") != "":
		u, err = user.Lookup(os.Getenv("SUDO_USER"))
	default:
		u, err = user.Current()
	}

	if err != nil {
		return Config{}, fmt.Errorf("не удалось определить пользователя: %w", err)
	}

	return Config{
		TargetUser: u.Username,
		TargetHome: u.HomeDir,
	}, nil
}

func getModuleDefs() []ModuleDef {
	return []ModuleDef{
		{
			ID:          "upgrade_ubuntu",
			Title:       "Обновление Ubuntu",
			Description: "full-upgrade + переход на последнюю LTS-версию и патчи ядра",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return false },
		},
		{
			ID:          "create_user",
			Title:       "Создать sudo-пользователя",
			Description: "новый аккаунт с паролем для RDP/sudo и перенос SSH-ключей из root",
			DefaultOn:   os.Getenv("SUDO_USER") == "",
			IsReady:     func(cfg Config) bool { return cfg.TargetUser != "root" },
		},
		{
			ID:          "update",
			Title:       "Базовые CLI-утилиты",
			Description: "curl, wget, git, htop, btop, tmux, unzip, net-tools, gnupg",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return false },
		},
		{
			ID:          "xfce",
			Title:       "Графическая оболочка XFCE",
			Description: "рабочий стол + удаление screensaver/light-locker (фикс черного экрана RDP)",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return hasBin("xfce4-session") },
		},
		{
			ID:          "xrdp",
			Title:       "Сервер XRDP",
			Description: "удаленный рабочий стол на 127.0.0.1:3389 (или через Tailscale) + фикс polkit",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return serviceActive("xrdp") },
		},
		{
			ID:          "kitty_fonts",
			Title:       "Терминал Kitty + Nerd Fonts",
			Description: "быстрый терминал по умолчанию и шрифты JetBrainsMono / FiraCode с иконками",
			DefaultOn:   true,
			IsReady: func(cfg Config) bool {
				return hasBin("kitty") && fileExists(filepath.Join(cfg.TargetHome, ".local", "share", "fonts", "JetBrainsMono"))
			},
		},
		{
			ID:          "ru_layout",
			Title:       "Русская раскладка",
			Description: "локаль ru_RU.UTF-8 и переключение US/RU по Alt+Shift в системе и XFCE",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return fileContains("/etc/default/keyboard", `"us,ru"`) },
		},
		{
			ID:          "zsh_starship",
			Title:       "Zsh + Starship",
			Description: "удобная оболочка, красивый промпт, автодополнение и подсветка команд",
			DefaultOn:   true,
			IsReady: func(cfg Config) bool {
				return hasBin("zsh") && hasBin("starship") && fileContains(filepath.Join(cfg.TargetHome, ".zshrc"), "# >>> vps-setup zsh >>>")
			},
		},
		{
			ID:          "bbr",
			Title:       "Сетевой алгоритм TCP BBR",
			Description: "ускорение сети в sysctl для плавного отклика интерфейса по RDP",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return fileExists("/etc/sysctl.d/99-bbr.conf") },
		},
		{
			ID:          "ssh_hardening",
			Title:       "Харденинг SSH",
			Description: "запрет входа под root и по паролю, смена стандартного порта (только ключи)",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return fileExists("/etc/ssh/sshd_config.d/99-hardening.conf") },
		},
		{
			ID:          "fail2ban_ufw",
			Title:       "Fail2Ban + файрвол UFW",
			Description: "бан за перебор паролей и закрытие всех портов, кроме SSH (и RDP в Tailscale)",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return serviceActive("fail2ban") && fileExists("/etc/fail2ban/jail.local") },
		},
		{
			ID:          "swap",
			Title:       "Swap-файл 2 ГБ",
			Description: "файл подкачки в /etc/fstab для стабильной работы UI на малом объеме RAM",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return fileExists("/swapfile") },
		},
		{
			ID:          "unattended",
			Title:       "Автообновления безопасности",
			Description: "фоновая установка критических патчей безопасности Ubuntu",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return fileExists("/etc/apt/apt.conf.d/20auto-upgrades") },
		},
		{
			ID:          "ai_cli",
			Title:       "AI CLI-инструменты",
			Description: "Claude Code, Codex, OpenCode, Gemini, Aider + Node.js 22, uv, gh, ripgrep",
			DefaultOn:   true,
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
			IsReady:     func(Config) bool { return hasBin("docker") && serviceActive("docker") },
		},
		{
			ID:          "tailscale",
			Title:       "Tailscale (mesh-VPN)",
			Description: "приватная шифрованная сеть для прямого входа по RDP без SSH-туннеля",
			DefaultOn:   false,
			IsReady:     func(Config) bool { return hasBin("tailscale") && serviceActive("tailscaled") },
		},
		{
			ID:          "ollama",
			Title:       "Ollama (локальные LLM)",
			Description: "запуск локальных ИИ-моделей на 127.0.0.1:11434 без отправки данных в облако",
			DefaultOn:   false,
			IsReady:     func(Config) bool { return hasBin("ollama") && serviceActive("ollama") },
		},
	}
}

func buildMenu(cfg Config) ([]huh.Option[string], []string) {
	defs := getModuleDefs()
	options := make([]huh.Option[string], 0, len(defs))
	var preselected []string

	nameStyle := lipgloss.NewStyle().Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	badgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575"))

	for _, m := range defs {
		label := nameStyle.Render(m.Title) + descStyle.Render(" — "+m.Description)
		installed := m.IsReady(cfg)

		if installed {
			label += badgeStyle.Render(" [установлено]")
		} else if m.DefaultOn {
			preselected = append(preselected, m.ID)
		}

		options = append(options, huh.NewOption(label, m.ID))
	}

	return options, preselected
}

func parseOnlyModules(raw string) ([]string, error) {
	valid := make(map[string]bool)
	for _, m := range getModuleDefs() {
		valid[m.ID] = true
	}

	var result []string
	seen := make(map[string]bool)

	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if !valid[id] {
			return nil, fmt.Errorf("неизвестный модуль: %q (доступные ID смотрите через --status)", id)
		}
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("флаг --only указан, но список модулей пуст")
	}
	return result, nil
}

func executeModule(mod string, cfg *Config) error {
	switch mod {
	case "upgrade_ubuntu":
		return runBash(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get update
			apt-get -o Dpkg::Options::="--force-confdef" -o Dpkg::Options::="--force-confold" full-upgrade -y
			apt-get install -y update-manager-core
			apt-get autoremove -y

			if [ -f /etc/update-manager/release-upgrades ]; then
				sed -i 's/^Prompt=.*/Prompt=lts/' /etc/update-manager/release-upgrades
			fi

			if do-release-upgrade -c >/dev/null 2>&1; then
				do-release-upgrade -f DistUpgradeViewNonInteractive
			fi
		`)

	case "create_user":
		username := strings.TrimSpace(cfg.NewUsername)
		if username == "" {
			return fmt.Errorf("не указано имя нового пользователя")
		}

		script := fmt.Sprintf(`
			if ! id -u "%s" >/dev/null 2>&1; then
				adduser --disabled-password --gecos "" "%s"
			fi
			usermod -aG sudo "%s"

			USER_HOME=$(eval echo "~%s")
			mkdir -p "$USER_HOME/.ssh"
			if [ -f /root/.ssh/authorized_keys ]; then
				cat /root/.ssh/authorized_keys >> "$USER_HOME/.ssh/authorized_keys"
				sort -u "$USER_HOME/.ssh/authorized_keys" -o "$USER_HOME/.ssh/authorized_keys"
			fi
			chmod 700 "$USER_HOME/.ssh"
			chmod 600 "$USER_HOME/.ssh/authorized_keys" 2>/dev/null || true
			chown -R "%s:%s" "$USER_HOME/.ssh"
		`, username, username, username, username, username, username)

		if err := runBash(script); err != nil {
			return err
		}

		if dryRun {
			fmt.Println(dryStyle.Render(fmt.Sprintf("  [DRY-RUN] Установка пароля через chpasswd для %s", username)))
			cfg.TargetUser = username
			cfg.TargetHome = filepath.Join("/home", username)
			return nil
		}

		if cfg.NewPassword != "" {
			cmd := exec.Command("chpasswd")
			cmd.Stdin = strings.NewReader(fmt.Sprintf("%s:%s\n", username, cfg.NewPassword))
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("ошибка установки пароля: %v (%s)", err, string(out))
			}
		}

		u, err := user.Lookup(username)
		if err != nil {
			return fmt.Errorf("не удалось прочитать профиль %s: %w", username, err)
		}
		cfg.TargetUser = u.Username
		cfg.TargetHome = u.HomeDir
		return nil

	case "update":
		return runBash(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get update
			apt-get upgrade -y
			apt-get install -y curl wget git htop btop tmux unzip fontconfig software-properties-common net-tools ca-certificates gnupg
		`)

	case "xfce":
		return runBash(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get install -y xfce4 xfce4-goodies dbus-x11 x11-xserver-utils
			apt-get purge -y xfce4-screensaver light-locker || true
			apt-get autoremove -y
		`)

	case "xrdp":
		xrdpPort := "tcp://127.0.0.1:3389"
		if contains(cfg.Modules, "tailscale") {
			xrdpPort = "3389"
		}

		if err := writeEmbeddedFile("configs/45-allow-colord.rules", "/etc/polkit-1/rules.d/45-allow-colord.rules", 0644); err != nil {
			return err
		}

		script := fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get install -y xrdp
			adduser xrdp ssl-cert || true

			sed -i -E 's|^port=.*|port=%s|g' /etc/xrdp/xrdp.ini

			echo "xfce4-session" > "%s/.xsession"
			chown "%s:%s" "%s/.xsession"

			if ! grep -q "unset DBUS_SESSION_BUS_ADDRESS" /etc/xrdp/startwm.sh; then
				sed -i '/test -x \/etc\/X11\/Xsession/i unset DBUS_SESSION_BUS_ADDRESS\nunset XDG_RUNTIME_DIR' /etc/xrdp/startwm.sh
			fi

			systemctl enable xrdp
			systemctl restart xrdp
		`, xrdpPort, cfg.TargetHome, cfg.TargetUser, cfg.TargetUser, cfg.TargetHome)
		return runBash(script)

	case "kitty_fonts":
		kittyConf := filepath.Join(cfg.TargetHome, ".config", "kitty", "kitty.conf")
		if _, err := os.Stat(kittyConf); os.IsNotExist(err) {
			if err := writeEmbeddedFile("configs/kitty.conf", kittyConf, 0644); err != nil {
				return err
			}
		}

		script := fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get install -y kitty unzip curl fontconfig

			mkdir -p "%s/.config/xfce4"
			echo "TerminalEmulator=kitty" > "%s/.config/xfce4/helpers.rc"

			FONT_DIR="%s/.local/share/fonts"
			mkdir -p "$FONT_DIR"

			for FONT in JetBrainsMono FiraCode; do
				if [ ! -d "$FONT_DIR/$FONT" ]; then
					mkdir -p "$FONT_DIR/$FONT"
					curl -fsSL "https://github.com/ryanoasis/nerd-fonts/releases/latest/download/${FONT}.zip" -o "/tmp/${FONT}.zip"
					unzip -oq "/tmp/${FONT}.zip" -d "$FONT_DIR/$FONT"
					rm -f "/tmp/${FONT}.zip"
				fi
			done

			chown -R "%s:%s" "%s/.config" "%s/.local"
			su - "%s" -c "fc-cache -f"
		`, cfg.TargetHome, cfg.TargetHome, cfg.TargetHome, cfg.TargetUser, cfg.TargetUser, cfg.TargetHome, cfg.TargetHome, cfg.TargetUser)
		return runBash(script)

	case "ru_layout":
		script := fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get install -y locales
			locale-gen ru_RU.UTF-8

			cat << 'EOF' > /etc/default/keyboard
XKBMODEL="pc105"
XKBLAYOUT="us,ru"
XKBVARIANT=","
XKBOPTIONS="grp:alt_shift_toggle"
BACKSPACE="guess"
EOF
			dpkg-reconfigure -f noninteractive keyboard-configuration || true

			mkdir -p "%s/.config/autostart"
			cat << 'EOF' > "%s/.config/autostart/setxkbmap.desktop"
[Desktop Entry]
Type=Application
Name=Keyboard Layout US/RU
Exec=setxkbmap -layout us,ru -option grp:alt_shift_toggle
Hidden=false
NoDisplay=false
X-GNOME-Autostart-enabled=true
EOF
			chown -R "%s:%s" "%s/.config/autostart"
		`, cfg.TargetHome, cfg.TargetHome, cfg.TargetUser, cfg.TargetUser, cfg.TargetHome)
		return runBash(script)

	case "zsh_starship":
		zshrcPath := filepath.Join(cfg.TargetHome, ".zshrc")
		if err := appendEmbeddedBlock("configs/zshrc.snippet", zshrcPath, "# >>> vps-setup zsh >>>"); err != nil {
			return err
		}

		script := fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get install -y zsh zsh-autosuggestions zsh-syntax-highlighting curl

			if ! command -v starship >/dev/null 2>&1; then
				curl -sS https://starship.rs/install.sh | sh -s -- -y
			fi

			chsh -s "$(which zsh)" "%s"

			mkdir -p "%s/.config"
			if [ ! -f "%s/.config/starship.toml" ]; then
				starship preset nerd-font-symbols -o "%s/.config/starship.toml"
			fi

			chown -R "%s:%s" "%s" "%s/.config"
		`, cfg.TargetUser, cfg.TargetHome, cfg.TargetHome, cfg.TargetHome, cfg.TargetUser, cfg.TargetUser, zshrcPath, cfg.TargetHome)
		return runBash(script)

	case "bbr":
		return runBash(`
			cat << 'EOF' > /etc/sysctl.d/99-bbr.conf
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
EOF
			sysctl --system
		`)

	case "ssh_hardening":
		authKeys := filepath.Join(cfg.TargetHome, ".ssh", "authorized_keys")
		info, err := os.Stat(authKeys)
		if err != nil || info.Size() == 0 {
			if dryRun {
				fmt.Println(warnStyle.Render(fmt.Sprintf("  [DRY-RUN] Внимание: %s не найден (в реальном запуске модуль остановится)", authKeys)))
			} else {
				return fmt.Errorf("файл %s не найден или пуст — добавьте SSH-ключ перед отключением паролей", authKeys)
			}
		}

		script := fmt.Sprintf(`
			mkdir -p /etc/ssh/sshd_config.d
			cat << 'EOF' > /etc/ssh/sshd_config.d/99-hardening.conf
Port %s
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
EOF
			sshd -t
			systemctl reload ssh || systemctl reload sshd
		`, cfg.SSHPort)
		return runBash(script)

	case "fail2ban_ufw":
		if err := runBash("export DEBIAN_FRONTEND=noninteractive && apt-get install -y fail2ban ufw"); err != nil {
			return err
		}

		if err := writeEmbeddedTemplate("configs/jail.local.tmpl", "/etc/fail2ban/jail.local", cfg, 0644); err != nil {
			return err
		}

		tailscaleRule := ""
		if contains(cfg.Modules, "tailscale") {
			tailscaleRule = "ufw allow in on tailscale0 to any port 3389 proto tcp"
		}

		script := fmt.Sprintf(`
			systemctl enable fail2ban
			systemctl restart fail2ban

			ufw default deny incoming
			ufw default allow outgoing
			ufw allow %s/tcp
			%s
			ufw --force enable
		`, cfg.SSHPort, tailscaleRule)
		return runBash(script)

	case "swap":
		return runBash(`
			if ! swapon --show | grep -q "/swapfile"; then
				fallocate -l 2G /swapfile
				chmod 600 /swapfile
				mkswap /swapfile
				swapon /swapfile
				grep -q "/swapfile" /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
			fi
		`)

	case "unattended":
		return runBash(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get install -y unattended-upgrades
			dpkg-reconfigure -f noninteractive unattended-upgrades
		`)

	case "ai_cli":
		script := fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get update
			apt-get install -y curl ca-certificates gnupg ripgrep fd-find jq gh

			if ! command -v node >/dev/null 2>&1 || [ "$(node -v | cut -d. -f1 | tr -d 'v')" -lt 20 ]; then
				curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
				apt-get install -y nodejs
			fi

			npm install -g \
				@anthropic-ai/claude-code \
				@openai/codex \
				opencode-ai \
				@google/gemini-cli

			su - "%s" -c '
				if ! command -v uv >/dev/null 2>&1 && [ ! -x "$HOME/.local/bin/uv" ]; then
					curl -LsSf https://astral.sh/uv/install.sh | sh
				fi
				export PATH="$HOME/.local/bin:$PATH"
				uv tool install --upgrade aider-chat
			'
		`, cfg.TargetUser)
		return runBash(script)

	case "docker":
		if _, err := os.Stat("/etc/docker/daemon.json"); os.IsNotExist(err) {
			if err := writeEmbeddedFile("configs/daemon.json", "/etc/docker/daemon.json", 0644); err != nil {
				return err
			}
		}

		script := fmt.Sprintf(`
			export DEBIAN_FRONTEND=noninteractive
			apt-get update
			apt-get install -y ca-certificates curl gnupg

			install -m 0755 -d /etc/apt/keyrings
			if [ ! -f /etc/apt/keyrings/docker.asc ]; then
				curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
				chmod a+r /etc/apt/keyrings/docker.asc
			fi

			echo \
			  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \
			  $(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}") stable" | \
			  tee /etc/apt/sources.list.d/docker.list > /dev/null

			apt-get update
			apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

			usermod -aG docker "%s"

			systemctl enable docker
			systemctl restart docker
		`, cfg.TargetUser)
		return runBash(script)

	case "tailscale":
		return runBash(`
			curl -fsSL https://tailscale.com/install.sh | sh
			systemctl enable tailscaled
			systemctl start tailscaled
		`)

	case "ollama":
		return runBash(`
			if ! command -v ollama >/dev/null 2>&1; then
				curl -fsSL https://ollama.com/install.sh | sh
			fi
			systemctl enable ollama
			systemctl restart ollama
		`)
	}
	return nil
}

// --- Статус и мониторинг ---

func collectStatuses(cfg Config) []ModuleStatus {
	var list []ModuleStatus
	for _, m := range getModuleDefs() {
		if m.ID == "update" || m.ID == "upgrade_ubuntu" {
			continue
		}
		details := moduleDetails(m.ID)
		if details == "—" {
			details = ""
		}
		list = append(list, ModuleStatus{
			ID:      m.ID,
			Title:   m.Title,
			Ready:   m.IsReady(cfg),
			Details: details,
		})
	}
	return list
}

func printStatusJSON(cfg Config) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(collectStatuses(cfg)); err != nil {
		fmt.Fprintf(os.Stderr, "json encode error: %v\n", err)
		os.Exit(1)
	}
}

func printStatusTable(cfg Config) {
	statuses := collectStatuses(cfg)
	rows := make([][]string, 0, len(statuses))

	for _, s := range statuses {
		statusText := "✖ Не установлен"
		if s.Ready {
			statusText = "✓ Активен"
		}
		details := s.Details
		if details == "" {
			details = "—"
		}
		rows = append(rows, []string{s.ID, s.Title, statusText, details})
	}

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4")).Padding(0, 1)
	cellStyle := lipgloss.NewStyle().Padding(0, 1)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#626262")).Padding(0, 1)

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("#44475A"))).
		Headers("ID", "Модуль", "Статус", "Детали").
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return headerStyle
			}
			if col == 2 {
				if strings.HasPrefix(rows[row][2], "✓") {
					return okStyle.Padding(0, 1)
				}
				return dimStyle
			}
			return cellStyle
		})

	fmt.Println(titleStyle.Render("🖥  Состояние модулей сервера"))
	fmt.Println(t.Render())
}

func moduleDetails(id string) string {
	switch id {
	case "xrdp":
		if fileContains("/etc/xrdp/xrdp.ini", "port=tcp://127.0.0.1:3389") {
			return "127.0.0.1:3389 (туннель)"
		}
		if serviceActive("xrdp") {
			return "0.0.0.0:3389 (UFW/Tailscale)"
		}
	case "tailscale":
		if out, err := exec.Command("tailscale", "ip", "-4").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	case "fail2ban_ufw":
		if serviceActive("ufw") {
			return "UFW активен"
		}
	case "docker":
		if out, err := exec.Command("docker", "--version").Output(); err == nil {
			parts := strings.Fields(string(out))
			if len(parts) >= 3 {
				return strings.TrimSuffix(parts[2], ",")
			}
		}
	case "ollama":
		if serviceActive("ollama") {
			return "127.0.0.1:11434"
		}
	}
	return "—"
}

// --- Самообновление ---

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func selfUpdate() error {
	fmt.Printf("Текущая версия: %s. Проверка обновлений...\n", Version)

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vps-setup-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API вернул статус %s", resp.Status)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return err
	}

	if rel.TagName == Version {
		fmt.Println(okStyle.Render("✓ У вас уже установлена последняя версия (" + Version + ")"))
		return nil
	}

	expectedAsset := fmt.Sprintf("vps-setup-%s-%s", runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, asset := range rel.Assets {
		if asset.Name == expectedAsset {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("файл %s не найден в релизе %s", expectedAsset, rel.TagName)
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return err
	}

	var dlErr error
	_ = spinner.New().
		Title(fmt.Sprintf("Скачивание %s (%s)...", rel.TagName, expectedAsset)).
		Action(func() {
			dlErr = downloadAndReplace(downloadURL, exePath)
		}).
		Run()

	if dlErr != nil {
		return dlErr
	}

	fmt.Println(okStyle.Render(fmt.Sprintf("✓ Успешно обновлено до версии %s", rel.TagName)))
	return nil
}

func downloadAndReplace(url, targetPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ошибка скачивания: %s", resp.Status)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(targetPath), ".vps-setup-update-*")
	if err != nil {
		return fmt.Errorf("нет прав на запись в %s (попробуйте через sudo): %w", filepath.Dir(targetPath), err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return err
	}
	return os.Rename(tmpPath, targetPath)
}

// --- Хелперы ФС и запуска ---

func writeEmbeddedFile(embedPath, destPath string, perm os.FileMode) error {
	if dryRun {
		fmt.Println(dryStyle.Render(fmt.Sprintf("  [DRY-RUN] Запись файла: %s (из %s, права %04o)", destPath, embedPath, perm)))
		return nil
	}
	data, err := configFS.ReadFile(embedPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(destPath, data, perm)
}

func writeEmbeddedTemplate(embedPath, destPath string, data any, perm os.FileMode) error {
	if dryRun {
		fmt.Println(dryStyle.Render(fmt.Sprintf("  [DRY-RUN] Рендер шаблона: %s (из %s, права %04o)", destPath, embedPath, perm)))
		return nil
	}
	tmpl, err := template.ParseFS(configFS, embedPath)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(destPath, buf.Bytes(), perm)
}

func appendEmbeddedBlock(embedPath, destPath, marker string) error {
	if dryRun {
		fmt.Println(dryStyle.Render(fmt.Sprintf("  [DRY-RUN] Добавление блока в: %s (из %s)", destPath, embedPath)))
		return nil
	}
	existing, err := os.ReadFile(destPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(existing), marker) {
		return nil
	}

	snippet, err := configFS.ReadFile(embedPath)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(destPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(snippet)
	return err
}

func runBash(script string) error {
	if dryRun {
		fmt.Println(dryStyle.Render("  [DRY-RUN] Выполнение bash-скрипта:"))
		for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				fmt.Printf("    %s\n", trimmed)
			}
		}
		return nil
	}

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Stdout = f
	cmd.Stderr = f
	return cmd.Run()
}

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func serviceActive(name string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", name).Run() == nil
}

func fileContains(path, substr string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), substr)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if strings.EqualFold(s, item) {
			return true
		}
	}
	return false
}
