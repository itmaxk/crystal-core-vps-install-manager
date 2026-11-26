package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
)

//go:embed configs/*
var configFS embed.FS

var (
	Version = "dev"
	dryRun  bool

	// logPath — переменная (а не константа), чтобы тесты могли писать лог во временный каталог.
	logPath = "/var/log/vps-setup.log"

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4")).MarginBottom(1)
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")).Bold(true)
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87")).Bold(true)
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C"))
	dryStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD"))
	hintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Bold(true)
)

const (
	githubRepo  = "itmaxk/crystal-core-vps-install-manager"
	passwordEnv = "VPS_SETUP_PASSWORD"

	presetRDP  = "rdp"
	presetFull = "full"

	hardeningConf       = "/etc/ssh/sshd_config.d/01-vps-setup-hardening.conf"
	legacyHardeningConf = "/etc/ssh/sshd_config.d/99-hardening.conf"
	minPasswordLen      = 8
)

// sshKeyRegex находит публичный ключ в строке authorized_keys, отбрасывая опции перед ним
// (например, command="echo 'Please login as the user ubuntu'..." в root-ключах облачных образов).
const sshKeyRegex = `(sk-)?(ssh-(rsa|dss|ed25519)|ecdsa-sha2-nistp(256|384|521))(@openssh\.com)? AAAA[A-Za-z0-9+/=]+`

var (
	usernameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	sshKeyRe   = regexp.MustCompile(sshKeyRegex)
)

// Модули быстрого старта: на «голом» сервере первым делом нужен рабочий стол и вход по RDP.
var rdpPresetModules = []string{"create_user", "swap", "xfce", "xrdp", "ru_layout"}

// Модули, которым не нужен apt.
var noAptModules = map[string]bool{"bbr": true, "swap": true, "ssh_hardening": true, "create_user": true}

type Config struct {
	TargetUser  string
	TargetHome  string
	SourceHome  string // домашний каталог пользователя, запустившего sudo (откуда копировать SSH-ключи)
	SSHPort     string // порт SSH, который выставит ssh_hardening
	NewUsername string
	Password    string // пароль нового пользователя или пароль для RDP-входа текущего
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
	doUpdate := flag.Bool("self-update", false, "Обновить бинарник из GitHub Releases (для приватного репозитория нужен GITHUB_TOKEN)")
	showStatus := flag.Bool("status", false, "Показать состояние модулей")
	asJSON := flag.Bool("json", false, "Вывести статус в формате JSON (используется с --status)")
	onlyFlag := flag.String("only", "", "Неинтерактивный запуск указанных модулей через запятую (например: xfce,xrdp,docker)")
	presetFlag := flag.String("preset", "", "Неинтерактивный запуск набора модулей: rdp (рабочий стол + RDP) или full (все рекомендуемые)")
	userFlag := flag.String("user", "", "Целевой пользователь (для запуска под root из cloud-init / Ansible)")
	newUserFlag := flag.String("new-user", "", "Имя нового sudo-пользователя для create_user в неинтерактивном режиме (пароль — в переменной "+passwordEnv+")")
	sshPortFlag := flag.String("ssh-port", "", "Порт SSH для ssh_hardening (по умолчанию — текущий порт sshd, без смены)")
	autoReboot := flag.Bool("auto-reboot", false, "Автоматически перезагрузить сервер в конце, если система этого требует")
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
			fatal("Ошибка обновления: " + err.Error())
		}
		return
	}

	if *showStatus {
		cfg, err := resolveUserConfig(*userFlag)
		if err != nil {
			fatal(err.Error())
		}
		if *asJSON {
			printStatusJSON(cfg)
		} else {
			printStatusTable(cfg)
		}
		return
	}

	if !dryRun && os.Geteuid() != 0 {
		fatal("Запустите установщик через sudo (sudo ./vps-setup)")
	}

	cfg, err := resolveUserConfig(*userFlag)
	if err != nil {
		fatal(err.Error())
	}
	cfg.SSHPort = strings.TrimSpace(*sshPortFlag)
	if cfg.SSHPort == "" {
		cfg.SSHPort = detectSSHPorts()[0]
	}
	cfg.NewUsername = strings.TrimSpace(*newUserFlag)
	cfg.Password = os.Getenv(passwordEnv)

	nonInteractive := *onlyFlag != "" || *presetFlag != ""
	if nonInteractive {
		var mods []string
		if *presetFlag != "" {
			p, err := presetModules(*presetFlag, cfg)
			if err != nil {
				fatal(err.Error())
			}
			mods = append(mods, p...)
		}
		if *onlyFlag != "" {
			o, err := parseOnlyModules(*onlyFlag)
			if err != nil {
				fatal(err.Error())
			}
			mods = append(mods, o...)
		}
		cfg.Modules = orderModules(mods)
		if len(cfg.Modules) == 0 {
			fmt.Println(okStyle.Render("✓ Все модули выбранного набора уже установлены — делать нечего."))
			return
		}
	} else {
		if err := runInteractiveForm(&cfg); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				fmt.Println(warnStyle.Render("Установка отменена."))
				os.Exit(0)
			}
			fatal("Ошибка интерактивного меню: " + err.Error())
		}
		cfg.Modules = orderModules(cfg.Modules)
	}

	if err := validatePlan(&cfg); err != nil {
		fatal(err.Error())
	}

	fmt.Println(titleStyle.Render("План: " + strings.Join(cfg.Modules, " → ")))

	appendLog(fmt.Sprintf("\n=== Crystal-Core VPS Install Manager %s — %s ===\nМодули: %s\n",
		Version, time.Now().Format(time.RFC3339), strings.Join(cfg.Modules, ",")))

	if needsApt(cfg.Modules) {
		err := withSpinner("Обновление списков пакетов (apt-get update)", func() error {
			return runBash(cfg, "apt-get update")
		})
		if err != nil {
			fatal(fmt.Sprintf("apt-get update завершился ошибкой: %v (подробности в %s)", err, logPath))
		}
	}

	var failed []string
	for i, mod := range cfg.Modules {
		title := fmt.Sprintf("[%d/%d] %s (%s)", i+1, len(cfg.Modules), moduleTitle(mod), mod)
		appendLog(fmt.Sprintf("\n### %s — %s\n", mod, time.Now().Format(time.RFC3339)))

		runErr := withSpinner(title, func() error { return executeModule(mod, &cfg) })
		if runErr != nil {
			fmt.Println(errStyle.Render(fmt.Sprintf("✖ %s — ошибка: %v", title, runErr)))
			appendLog(fmt.Sprintf("!!! Ошибка в модуле %s: %v\n", mod, runErr))
			failed = append(failed, mod)
			if mod == "create_user" {
				fatal("Остальные модули настраиваются для нового пользователя — дальнейшая установка остановлена. Подробности: " + logPath)
			}
			continue
		}
		fmt.Println(okStyle.Render("✓ " + title))
		afterModule(mod, cfg)
	}

	fmt.Println()
	if len(failed) > 0 {
		fmt.Println(errStyle.Render("Завершено с ошибками в модулях: " + strings.Join(failed, ", ")))
		fmt.Println(warnStyle.Render("Подробности смотрите в " + logPath + ", исправьте причину и запустите эти модули снова: sudo vps-setup --only " + strings.Join(failed, ",")))
	} else {
		fmt.Println(okStyle.Render("Готово! Все выбранные модули обработаны."))
	}

	if contains(cfg.Modules, "xrdp") && !contains(failed, "xrdp") {
		printRDPHint(cfg)
	}
	if contains(cfg.Modules, "tailscale") && !contains(failed, "tailscale") {
		fmt.Println(warnStyle.Render("Для активации Tailscale выполните: sudo tailscale up"))
	}
	if !dryRun && cfg.TargetUser != "root" && contains(cfg.Modules, "xrdp") && !userHasPassword(cfg.TargetUser) {
		fmt.Println(warnStyle.Render(fmt.Sprintf("⚠ У пользователя %s не задан пароль — войти по RDP не получится. Задайте его: sudo passwd %s", cfg.TargetUser, cfg.TargetUser)))
	}

	handleReboot(cfg, *autoReboot)

	if len(failed) > 0 {
		os.Exit(1)
	}
}

func fatal(msg string) {
	fmt.Println(errStyle.Render("✖ " + msg))
	os.Exit(1)
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

	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "root" {
		sudoUser = ""
	}

	switch {
	case explicitUser != "":
		u, err = user.Lookup(explicitUser)
		if err != nil {
			return Config{}, fmt.Errorf("пользователь %q не найден в системе: %w", explicitUser, err)
		}
	case sudoUser != "":
		u, err = user.Lookup(sudoUser)
	default:
		u, err = user.Current()
	}
	if err != nil {
		return Config{}, fmt.Errorf("не удалось определить пользователя: %w", err)
	}

	cfg := Config{TargetUser: u.Username, TargetHome: u.HomeDir}
	if sudoUser != "" {
		if su, err := user.Lookup(sudoUser); err == nil {
			cfg.SourceHome = su.HomeDir
		}
	}
	return cfg, nil
}

// getModuleDefs возвращает модули в порядке выполнения: сначала пользователь,
// рабочий стол и RDP (чтобы как можно раньше можно было подключиться), затем всё остальное.
func getModuleDefs() []ModuleDef {
	return []ModuleDef{
		{
			ID:          "create_user",
			Title:       "Создать sudo-пользователя",
			Description: "новый аккаунт с паролем для RDP/sudo и перенос SSH-ключей",
			DefaultOn:   true,
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
			IsReady:     func(Config) bool { return swapActive("/swapfile") },
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
			ID:          "ru_layout",
			Title:       "Русская раскладка",
			Description: "локаль ru_RU.UTF-8 и переключение US/RU по Alt+Shift в системе и XFCE",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return fileContains("/etc/default/keyboard", `"us,ru"`) },
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
			ID:          "tailscale",
			Title:       "Tailscale (mesh-VPN)",
			Description: "приватная шифрованная сеть для прямого входа по RDP без SSH-туннеля",
			DefaultOn:   false,
			IsReady:     func(Config) bool { return hasBin("tailscale") && serviceActive("tailscaled") },
		},
		{
			ID:          "ssh_hardening",
			Title:       "Харденинг SSH",
			Description: "запрет входа под root и по паролю (только ключи), опционально смена порта",
			DefaultOn:   true,
			IsReady: func(Config) bool {
				return fileExists(hardeningConf) || fileExists(legacyHardeningConf)
			},
		},
		{
			ID:          "fail2ban_ufw",
			Title:       "Fail2Ban + файрвол UFW",
			Description: "бан за перебор паролей и закрытие всех портов, кроме SSH (и RDP в Tailscale)",
			DefaultOn:   true,
			IsReady:     func(Config) bool { return serviceActive("fail2ban") && fileExists("/etc/fail2ban/jail.local") },
		},
		{
			ID:          "unattended",
			Title:       "Автообновления безопасности",
			Description: "фоновая установка критических патчей безопасности Ubuntu",
			DefaultOn:   true,
			IsReady: func(Config) bool {
				return hasBin("unattended-upgrade") && fileContains("/etc/apt/apt.conf.d/20auto-upgrades", `Unattended-Upgrade "1"`)
			},
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
			ID:          "ollama",
			Title:       "Ollama (локальные LLM)",
			Description: "запуск локальных ИИ-моделей на 127.0.0.1:11434 без отправки данных в облако",
			DefaultOn:   false,
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

func moduleTitle(id string) string {
	for _, m := range getModuleDefs() {
		if m.ID == id {
			return m.Title
		}
	}
	return id
}

func buildMenu(cfg Config) []huh.Option[string] {
	defs := getModuleDefs()
	options := make([]huh.Option[string], 0, len(defs))

	nameStyle := lipgloss.NewStyle().Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	badgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575"))

	for _, m := range defs {
		label := nameStyle.Render(m.Title) + descStyle.Render(" — "+m.Description)
		if m.IsReady(cfg) {
			label += badgeStyle.Render(" [установлено]")
		}
		options = append(options, huh.NewOption(label, m.ID))
	}
	return options
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
	valid := make(map[string]bool)
	for _, m := range getModuleDefs() {
		valid[m.ID] = true
	}

	var result []string
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if !valid[id] {
			return nil, fmt.Errorf("неизвестный модуль: %q (доступные ID смотрите через --status)", id)
		}
		result = append(result, id)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("флаг --only указан, но список модулей пуст")
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

func runInteractiveForm(cfg *Config) error {
	preset := presetFull
	if !hasBin("xfce4-session") || !serviceActive("xrdp") {
		preset = presetRDP
	}

	err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Что настраиваем?").
			Description("Дальше можно будет поправить список модулей").
			Options(
				huh.NewOption("Быстрый старт — рабочий стол XFCE + вход по RDP (для первого запуска)", presetRDP),
				huh.NewOption("Полная настройка — все рекомендуемые модули (рабочий стол и RDP ставятся первыми)", presetFull),
			).
			Value(&preset),
	)).Run()
	if err != nil {
		return err
	}

	cfg.Modules, _ = presetModules(preset, *cfg)
	needRDPPassword := cfg.TargetUser != "root" && !userHasPassword(cfg.TargetUser)
	var confirmNew, confirmRDP string

	return huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Выберите модули для установки / обновления").
				Description("Пробел — отметить/снять, ↑/↓ — навигация, Enter — далее. Модули выполняются сверху вниз.").
				Height(19).
				Options(buildMenu(*cfg)...).
				Value(&cfg.Modules).
				Validate(func(v []string) error {
					if len(v) == 0 {
						return fmt.Errorf("отметьте хотя бы один модуль")
					}
					return nil
				}),
		),
		huh.NewGroup(
			huh.NewInput().
				Title("Имя нового пользователя").
				Description("Строчные латинские буквы, цифры, _ и - (например: dev, admin)").
				Value(&cfg.NewUsername).
				Validate(validateUsername),
			huh.NewInput().
				Title("Пароль пользователя (для sudo и входа по RDP)").
				Description("Вход по SSH останется по ключам, пароль нужен для sudo и RDP").
				EchoMode(huh.EchoModePassword).
				Value(&cfg.Password).
				Validate(validatePassword),
			huh.NewInput().
				Title("Повторите пароль").
				EchoMode(huh.EchoModePassword).
				Value(&confirmNew).
				Validate(func(s string) error {
					if s != cfg.Password {
						return fmt.Errorf("пароли не совпадают")
					}
					return nil
				}),
		).WithHideFunc(func() bool {
			return !contains(cfg.Modules, "create_user")
		}),
		huh.NewGroup(
			huh.NewInput().
				Title(fmt.Sprintf("Пароль для входа по RDP пользователя %s", cfg.TargetUser)).
				Description("У пользователя сейчас нет пароля, а RDP-вход возможен только по паролю").
				EchoMode(huh.EchoModePassword).
				Value(&cfg.Password).
				Validate(validatePassword),
			huh.NewInput().
				Title("Повторите пароль").
				EchoMode(huh.EchoModePassword).
				Value(&confirmRDP).
				Validate(func(s string) error {
					if s != cfg.Password {
						return fmt.Errorf("пароли не совпадают")
					}
					return nil
				}),
		).WithHideFunc(func() bool {
			return !needRDPPassword || !contains(cfg.Modules, "xrdp") || contains(cfg.Modules, "create_user")
		}),
		huh.NewGroup(
			huh.NewInput().
				Title("Порт для SSH").
				Description("Оставьте текущий, если не уверены. Новый порт должен быть открыт во внешнем файрволе\nпровайдера (Security List в Oracle Cloud, Security Group в AWS и т.п.), иначе доступ пропадёт.").
				Value(&cfg.SSHPort).
				Validate(validatePort),
		).WithHideFunc(func() bool {
			return !contains(cfg.Modules, "ssh_hardening")
		}),
	).Run()
}

func validateUsername(s string) error {
	s = strings.TrimSpace(s)
	if s == "root" || !usernameRe.MatchString(s) {
		return fmt.Errorf("укажите корректное имя пользователя: строчные латинские буквы, цифры, _ и - (не root)")
	}
	return nil
}

func validatePassword(s string) error {
	if len(s) < minPasswordLen {
		return fmt.Errorf("пароль должен быть не короче %d символов", minPasswordLen)
	}
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("пароль не должен содержать перевод строки")
	}
	return nil
}

func validatePort(s string) error {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("укажите корректный порт (1–65535)")
	}
	return nil
}

// validatePlan проверяет выбор до начала установки, чтобы не упасть через 20 минут
// посреди процесса и не оставить сервер без доступа.
func validatePlan(cfg *Config) error {
	if len(cfg.Modules) == 0 {
		return fmt.Errorf("не выбрано ни одного модуля")
	}
	cfg.SSHPort = strings.TrimSpace(cfg.SSHPort)
	if err := validatePort(cfg.SSHPort); err != nil {
		return fmt.Errorf("некорректный --ssh-port: %w", err)
	}

	finalUser := cfg.TargetUser
	if contains(cfg.Modules, "create_user") {
		cfg.NewUsername = strings.TrimSpace(cfg.NewUsername)
		if err := validateUsername(cfg.NewUsername); err != nil {
			return fmt.Errorf("create_user: %w (в неинтерактивном режиме укажите --new-user)", err)
		}
		if err := validatePassword(cfg.Password); err != nil {
			return fmt.Errorf("create_user: %w — без пароля не будет работать sudo и вход по RDP (передайте его в переменной %s)", err, passwordEnv)
		}
		finalUser = cfg.NewUsername
	}

	if contains(cfg.Modules, "ssh_hardening") {
		if finalUser == "root" {
			return fmt.Errorf("ssh_hardening запрещает вход под root, а другого пользователя нет — добавьте модуль create_user или укажите --user")
		}
		if contains(cfg.Modules, "create_user") {
			if !hasValidKeyFile(filepath.Join("/root", ".ssh", "authorized_keys")) &&
				!(cfg.SourceHome != "" && hasValidKeyFile(filepath.Join(cfg.SourceHome, ".ssh", "authorized_keys"))) &&
				!hasValidKeyFile(filepath.Join("/home", cfg.NewUsername, ".ssh", "authorized_keys")) {
				return dryRunTolerant(fmt.Errorf("ssh_hardening: не найдено SSH-ключей для переноса новому пользователю — после отключения паролей войти будет нельзя"))
			}
		} else if !hasValidKeyFile(filepath.Join(cfg.TargetHome, ".ssh", "authorized_keys")) {
			return dryRunTolerant(fmt.Errorf("ssh_hardening: в %s/.ssh/authorized_keys нет ни одного SSH-ключа — добавьте ключ перед отключением паролей", cfg.TargetHome))
		}
	}

	if contains(cfg.Modules, "xrdp") && !contains(cfg.Modules, "create_user") && cfg.Password != "" {
		if err := validatePassword(cfg.Password); err != nil {
			return fmt.Errorf("пароль для RDP: %w", err)
		}
	}

	if contains(cfg.Modules, "xrdp") && contains(cfg.Modules, "tailscale") && !contains(cfg.Modules, "fail2ban_ufw") && !ufwEnabled() {
		fmt.Println(warnStyle.Render("⚠ Внимание: выбран Tailscale + XRDP без UFW — RDP будет слушать все интерфейсы. Убедитесь, что порт 3389 закрыт внешним файрволом."))
	}
	return nil
}

func dryRunTolerant(err error) error {
	if dryRun {
		fmt.Println(warnStyle.Render("  [DRY-RUN] " + err.Error() + " (в реальном запуске установка не начнётся)"))
		return nil
	}
	return err
}

// afterModule выводит подсказки сразу после модуля, не дожидаясь окончания всей установки.
func afterModule(mod string, cfg Config) {
	switch mod {
	case "xrdp":
		if len(cfg.Modules) > 1 && cfg.Modules[len(cfg.Modules)-1] != "xrdp" {
			fmt.Println(hintStyle.Render("  RDP уже работает — можно подключаться, пока ставятся остальные модули (инструкция будет и в конце)."))
			printRDPHint(cfg)
		}
	case "ssh_hardening":
		fmt.Println(warnStyle.Render(fmt.Sprintf("  ⚠ Не закрывайте текущую SSH-сессию! Проверьте в новом окне: ssh -p %s %s@<IP_СЕРВЕРА>", cfg.SSHPort, cfg.TargetUser)))
	}
}

func printRDPHint(cfg Config) {
	port := detectSSHPorts()[0]
	fmt.Println()
	fmt.Println(hintStyle.Render("Подключение к рабочему столу по RDP"))
	if strings.Contains(xrdpListenPort(), "127.0.0.1") || dryRun && !contains(cfg.Modules, "tailscale") {
		fmt.Println("  1. На своём компьютере откройте SSH-туннель (окно не закрывайте):")
		fmt.Printf("       ssh -p %s -N -L 13389:127.0.0.1:3389 %s@<IP_СЕРВЕРА>\n", port, cfg.TargetUser)
		fmt.Println("  2. В RDP-клиенте (mstsc / Windows App / Remmina) подключитесь к localhost:13389")
	} else {
		fmt.Println("  1. Подключитесь RDP-клиентом к <TAILSCALE_IP>:3389 (адрес: tailscale ip -4)")
	}
	fmt.Printf("  3. Сессия Xorg, логин %s и пароль этого пользователя\n\n", cfg.TargetUser)
}

func handleReboot(cfg Config, autoReboot bool) {
	if dryRun || !fileExists("/var/run/reboot-required") {
		return
	}
	if autoReboot {
		fmt.Println(warnStyle.Render("⚠ Требуется перезагрузка. Сервер перезагрузится через 5 секунд..."))
		if err := exec.Command("systemd-run", "--on-active=5s", "--quiet", "systemctl", "reboot").Run(); err != nil {
			_ = exec.Command("systemctl", "reboot").Run()
		}
		return
	}
	fmt.Println(warnStyle.Render("⚠ Обновились ядро или системные библиотеки — требуется перезагрузка: sudo reboot"))
	if contains(cfg.Modules, "upgrade_ubuntu") {
		fmt.Println(warnStyle.Render("  После перезагрузки запустите sudo vps-setup --only upgrade_ubuntu, чтобы завершить переход на новую LTS."))
	}
}

// --- Модули ---

func executeModule(mod string, cfg *Config) error {
	switch mod {
	case "create_user":
		return moduleCreateUser(cfg)

	case "update":
		return runBash(*cfg, `
			apt-get upgrade
			apt-get install curl wget git htop btop tmux unzip fontconfig software-properties-common net-tools ca-certificates gnupg
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
			apt-get install xfce4 xfce4-goodies dbus-x11 x11-xserver-utils
			apt-get purge xfce4-screensaver light-locker || true
			apt-get autoremove

			# У VPS нет монитора: локальный дисплей-менеджер не нужен и только съедает RAM.
			systemctl set-default multi-user.target
			for dm in lightdm gdm3 sddm; do
				if systemctl is-enabled --quiet "$dm" 2>/dev/null; then
					systemctl disable --now "$dm" || true
				fi
			done
		`)

	case "xrdp":
		return moduleXRDP(cfg)

	case "ru_layout":
		return runBash(*cfg, `
			fix_user_dirs
			apt-get install locales keyboard-configuration
			locale-gen en_US.UTF-8 ru_RU.UTF-8

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
		return runBash(*cfg, `
			fix_user_dirs
			apt-get install kitty unzip curl fontconfig

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
X-XFCE-CommandsWithParameter=kitty -- "%s"
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
			apt-get install zsh zsh-autosuggestions zsh-syntax-highlighting curl

			if ! command -v starship >/dev/null 2>&1; then
				curl -fsSL https://starship.rs/install.sh | sh -s -- -y
			fi

			chsh -s "$(command -v zsh)" "$TARGET_USER"

			as_user mkdir -p "$TARGET_HOME/.config"
			if [ ! -f "$TARGET_HOME/.config/starship.toml" ]; then
				as_user starship preset nerd-font-symbols -o "$TARGET_HOME/.config/starship.toml"
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
			fi
			systemctl enable --now tailscaled
		`)

	case "ssh_hardening":
		return moduleSSHHardening(cfg)

	case "fail2ban_ufw":
		return moduleFail2banUFW(cfg)

	case "unattended":
		return runBash(*cfg, `
			apt-get install unattended-upgrades
			cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
			systemctl enable --now unattended-upgrades || true
		`)

	case "ai_cli":
		return runBash(*cfg, `
			apt-get install curl ca-certificates gnupg ripgrep fd-find jq

			# GitHub CLI из официального репозитория (в universe лежит сильно устаревшая версия).
			if ! command -v gh >/dev/null 2>&1; then
				install -m 0755 -d /etc/apt/keyrings
				curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg
				chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
				echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list
				apt-get update
				apt-get install gh
			fi

			if ! command -v node >/dev/null 2>&1 || [ "$(node -v | cut -d. -f1 | tr -d 'v')" -lt 20 ]; then
				curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
				apt-get install nodejs
			fi

			npm install -g \
				@anthropic-ai/claude-code \
				@openai/codex \
				opencode-ai \
				@google/gemini-cli

			fix_user_dirs
			as_user sh -c '
				if ! command -v uv >/dev/null 2>&1; then
					curl -LsSf https://astral.sh/uv/install.sh | sh
				fi
				uv tool install --python 3.12 --upgrade aider-chat
			'
		`)

	case "docker":
		if !fileExists("/etc/docker/daemon.json") {
			if err := writeEmbeddedFile("configs/daemon.json", "/etc/docker/daemon.json", 0644); err != nil {
				return err
			}
		}
		return runBash(*cfg, `
			apt-get install ca-certificates curl gnupg

			install -m 0755 -d /etc/apt/keyrings
			if [ ! -f /etc/apt/keyrings/docker.asc ]; then
				curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
				chmod a+r /etc/apt/keyrings/docker.asc
			fi

			CODENAME=$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")
			echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $CODENAME stable" > /etc/apt/sources.list.d/docker.list

			apt-get update
			apt-get install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

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

func moduleXRDP(cfg *Config) error {
	xrdpPort := "tcp://127.0.0.1:3389"
	if contains(cfg.Modules, "tailscale") {
		xrdpPort = "3389"
	}

	if err := writeEmbeddedFile("configs/45-allow-colord.rules", "/etc/polkit-1/rules.d/45-allow-colord.rules", 0644); err != nil {
		return err
	}
	if err := writeEmbeddedFile("configs/45-allow-colord.pkla", "/etc/polkit-1/localauthority/50-local.d/45-allow-colord.pkla", 0644); err != nil {
		return err
	}

	err := runBash(*cfg, `
		fix_user_dirs
		apt-get install xrdp xorgxrdp
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

	// Если порт меняется, а входящий трафик режет iptables (как в образах Oracle Cloud)
	// или уже включён UFW — сначала открываем новый порт, иначе новые подключения не пройдут.
	return runBash(*cfg, `
		mkdir -p /etc/ssh/sshd_config.d /run/sshd
		CURRENT_PORTS=$(sshd -T 2>/dev/null | awk '$1=="port"{print $2}' | tr '\n' ' ' || true)

		case " $CURRENT_PORTS " in
			*" $SSH_PORT "*) ;;
			*)
				if command -v ufw >/dev/null 2>&1 && grep -q "Status: active" <<<"$(ufw status)"; then
					ufw allow "$SSH_PORT/tcp"
				fi
				if command -v iptables >/dev/null 2>&1 && grep -qE -- '-j (REJECT|DROP)|^-P INPUT DROP' <<<"$(iptables -S INPUT 2>/dev/null || true)"; then
					iptables -C INPUT -p tcp --dport "$SSH_PORT" -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p tcp --dport "$SSH_PORT" -j ACCEPT
					if command -v netfilter-persistent >/dev/null 2>&1; then netfilter-persistent save; fi
				fi
				;;
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

		# Ubuntu 22.10+: sshd запускается через ssh.socket, порт берётся из генератора systemd.
		if systemctl is-enabled --quiet ssh.socket 2>/dev/null || systemctl is-active --quiet ssh.socket 2>/dev/null; then
			systemctl daemon-reload
			systemctl restart ssh.socket
			if systemctl is-active --quiet ssh.service; then
				systemctl restart ssh.service
			fi
		else
			systemctl restart ssh 2>/dev/null || systemctl restart sshd
		fi
	`, "SSH_PORT="+cfg.SSHPort, "HARDENING_CONF="+hardeningConf, "LEGACY_CONF="+legacyHardeningConf)
}

func moduleFail2banUFW(cfg *Config) error {
	if err := runBash(*cfg, "apt-get install fail2ban ufw python3-systemd"); err != nil {
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

// --- Статус и мониторинг ---

func collectStatuses(cfg Config) []ModuleStatus {
	var list []ModuleStatus
	for _, m := range getModuleDefs() {
		if m.ID == "update" || m.ID == "upgrade_ubuntu" {
			continue
		}
		list = append(list, ModuleStatus{
			ID:      m.ID,
			Title:   m.Title,
			Ready:   m.IsReady(cfg),
			Details: moduleDetails(m.ID),
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
		port := xrdpListenPort()
		if strings.Contains(port, "127.0.0.1") {
			return "127.0.0.1:3389 (через SSH-туннель)"
		}
		if serviceActive("xrdp") {
			return "0.0.0.0:3389 (UFW/Tailscale)"
		}
	case "tailscale":
		if out, err := exec.Command("tailscale", "ip", "-4").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	case "fail2ban_ufw":
		if ufwEnabled() {
			return "UFW активен"
		}
	case "ssh_hardening":
		if fileExists(hardeningConf) || fileExists(legacyHardeningConf) {
			return "порт " + strings.Join(detectSSHPorts(), ",")
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
	return ""
}

// --- Самообновление ---

type githubAsset struct {
	Name               string `json:"name"`
	URL                string `json:"url"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

func githubToken() string {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GH_TOKEN")
}

func githubGet(url, accept string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "vps-setup-updater")
	req.Header.Set("Accept", accept)
	if t := githubToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && githubToken() == "" {
			return nil, fmt.Errorf("GitHub вернул 404 — для приватного репозитория задайте GITHUB_TOKEN (sudo -E env GITHUB_TOKEN=... vps-setup --self-update)")
		}
		return nil, fmt.Errorf("GitHub вернул статус %s для %s", resp.Status, url)
	}
	return resp, nil
}

// assetDownloadURL: для приватного репозитория скачивать нужно через API с токеном.
func assetDownloadURL(a githubAsset) (string, string) {
	if githubToken() != "" && a.URL != "" {
		return a.URL, "application/octet-stream"
	}
	return a.BrowserDownloadURL, "application/octet-stream"
}

func selfUpdate() error {
	fmt.Printf("Текущая версия: %s. Проверка обновлений...\n", Version)

	resp, err := githubGet(fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo), "application/vnd.github+json", 30*time.Second)
	if err != nil {
		return fmt.Errorf("не удалось получить данные о релизе: %w", err)
	}
	var rel githubRelease
	err = json.NewDecoder(resp.Body).Decode(&rel)
	resp.Body.Close()
	if err != nil {
		return err
	}

	if rel.TagName == Version {
		fmt.Println(okStyle.Render("✓ У вас уже установлена последняя версия (" + Version + ")"))
		return nil
	}

	expectedAsset := fmt.Sprintf("vps-setup-%s-%s", runtime.GOOS, runtime.GOARCH)
	var binAsset, sumAsset *githubAsset
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case expectedAsset:
			binAsset = &rel.Assets[i]
		case "checksums.txt":
			sumAsset = &rel.Assets[i]
		}
	}
	if binAsset == nil {
		return fmt.Errorf("файл %s не найден в релизе %s", expectedAsset, rel.TagName)
	}

	var expectedSum string
	if sumAsset != nil {
		url, accept := assetDownloadURL(*sumAsset)
		r, err := githubGet(url, accept, 30*time.Second)
		if err != nil {
			return fmt.Errorf("не удалось скачать checksums.txt: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
		if err != nil {
			return err
		}
		expectedSum = findChecksum(string(data), expectedAsset)
		if expectedSum == "" {
			return fmt.Errorf("в checksums.txt нет контрольной суммы для %s", expectedAsset)
		}
	} else {
		fmt.Println(warnStyle.Render("⚠ В релизе нет checksums.txt — контрольная сумма не проверяется"))
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return err
	}

	url, accept := assetDownloadURL(*binAsset)
	err = withSpinner(fmt.Sprintf("Скачивание %s (%s)", rel.TagName, expectedAsset), func() error {
		return downloadAndReplace(url, accept, exePath, expectedSum)
	})
	if err != nil {
		return err
	}

	fmt.Println(okStyle.Render(fmt.Sprintf("✓ Успешно обновлено до версии %s", rel.TagName)))
	return nil
}

func findChecksum(checksums, name string) string {
	for _, line := range strings.Split(checksums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

func downloadAndReplace(url, accept, targetPath, expectedSum string) error {
	resp, err := githubGet(url, accept, 10*time.Minute)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	tmpFile, err := os.CreateTemp(filepath.Dir(targetPath), ".vps-setup-update-*")
	if err != nil {
		return fmt.Errorf("нет прав на запись в %s (попробуйте через sudo): %w", filepath.Dir(targetPath), err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, h), resp.Body); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if expectedSum != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != expectedSum {
			return fmt.Errorf("контрольная сумма не совпала (ожидалась %s, получена %s) — обновление отменено", expectedSum, got)
		}
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

// writeUserFile пишет файл в домашний каталог пользователя так, чтобы и файл, и все
// созданные по пути каталоги (например, ~/.config) принадлежали пользователю, а не root.
func writeUserFile(cfg Config, embedPath, destPath string, perm os.FileMode, overwrite bool) error {
	if !overwrite && fileExists(destPath) {
		return nil
	}
	if dryRun {
		fmt.Println(dryStyle.Render(fmt.Sprintf("  [DRY-RUN] Запись файла пользователя %s: %s (из %s)", cfg.TargetUser, destPath, embedPath)))
		return nil
	}
	uid, gid, err := lookupIDs(cfg.TargetUser)
	if err != nil {
		return err
	}
	data, err := configFS.ReadFile(embedPath)
	if err != nil {
		return err
	}
	if err := mkdirAllOwned(filepath.Dir(destPath), uid, gid); err != nil {
		return err
	}
	if err := os.WriteFile(destPath, data, perm); err != nil {
		return err
	}
	return os.Lchown(destPath, uid, gid)
}

func chownToUser(cfg Config, path string) error {
	if dryRun {
		return nil
	}
	uid, gid, err := lookupIDs(cfg.TargetUser)
	if err != nil {
		return err
	}
	return os.Lchown(path, uid, gid)
}

func lookupIDs(username string) (int, int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, 0, fmt.Errorf("пользователь %s не найден: %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// mkdirAllOwned создаёт недостающие каталоги и выставляет владельца только им.
func mkdirAllOwned(dir string, uid, gid int) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s существует и не является каталогом", dir)
		}
		return nil
	}
	if parent := filepath.Dir(dir); parent != dir {
		if err := mkdirAllOwned(parent, uid, gid); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0755); err != nil && !os.IsExist(err) {
		return err
	}
	return os.Lchown(dir, uid, gid)
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

// bashPreamble добавляется к каждому скрипту:
//   - apt-get всегда неинтерактивен и ждёт освобождения dpkg-lock (на свежем VPS
//     первые минуты работает unattended-upgrades), needrestart не задаёт вопросов;
//   - as_user выполняет команду от имени целевого пользователя в его HOME;
//   - fix_user_dirs чинит владельца ~/.config и т.п., если их когда-то создал root.
const bashPreamble = `
export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a
apt-get() {
	command apt-get -y -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "$@"
}
as_user() {
	( cd "$TARGET_HOME" && runuser -u "$TARGET_USER" -- env HOME="$TARGET_HOME" USER="$TARGET_USER" LOGNAME="$TARGET_USER" PATH="$TARGET_HOME/.local/bin:$PATH" "$@" )
}
fix_user_dirs() {
	if [ "$TARGET_USER" = root ]; then
		return 0
	fi
	local d
	for d in .config .local .local/share .local/bin .cache .config/autostart .config/xfce4; do
		if [ -d "$TARGET_HOME/$d" ]; then
			chown "$TARGET_USER:" "$TARGET_HOME/$d"
		fi
	done
}
`

func runBash(cfg Config, script string, env ...string) error {
	if dryRun {
		fmt.Println(dryStyle.Render("  [DRY-RUN] Выполнение bash-скрипта:"))
		for _, e := range env {
			fmt.Printf("    export %s\n", e)
		}
		for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				fmt.Printf("    %s\n", trimmed)
			}
		}
		return nil
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	cmd := exec.Command("bash", "-euo", "pipefail", "-c", bashPreamble+script)
	cmd.Env = append(os.Environ(), "TARGET_USER="+cfg.TargetUser, "TARGET_HOME="+cfg.TargetHome)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout = f
	cmd.Stderr = f
	// stdin = /dev/null: ни одна команда не должна зависнуть, ожидая ответа.
	return cmd.Run()
}

func appendLog(text string) {
	if dryRun {
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(text)
}

// withSpinner показывает индикатор с таймером, пока выполняется fn. Без терминала
// (cloud-init, Ansible, перенаправленный вывод) и в dry-run просто печатает заголовок.
func withSpinner(title string, fn func() error) error {
	if dryRun || !isTerminal(os.Stdout) {
		fmt.Println("--> " + title)
		return fn()
	}

	done := make(chan error, 1)
	go func() { done <- fn() }()

	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	start := time.Now()
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()

	for i := 0; ; i++ {
		select {
		case err := <-done:
			fmt.Print("\r\033[K")
			return err
		case <-ticker.C:
			elapsed := time.Since(start).Round(time.Second)
			fmt.Printf("\r\033[K%s %s %s", dryStyle.Render(string(frames[i%len(frames)])), title,
				lipgloss.NewStyle().Foreground(lipgloss.Color("#626262")).Render(elapsed.String()))
		}
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func setPassword(username, password string) error {
	if password == "" {
		return nil
	}
	cmd := exec.Command("chpasswd")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("%s:%s\n", username, password))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ошибка установки пароля: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// userHasPassword: true, если у пользователя есть рабочий пароль (passwd -S → "P").
// При ошибке считаем, что пароль есть, чтобы не задавать лишних вопросов.
func userHasPassword(username string) bool {
	out, err := exec.Command("passwd", "-S", username).Output()
	if err != nil {
		return true
	}
	f := strings.Fields(string(out))
	return len(f) < 2 || f[1] == "P"
}

func hasValidKeyFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") && sshKeyRe.MatchString(line) {
			return true
		}
	}
	return false
}

// detectSSHPorts возвращает порты, на которых настроен sshd (минимум один элемент).
func detectSSHPorts() []string {
	if os.Geteuid() == 0 {
		_ = os.MkdirAll("/run/sshd", 0755)
	}
	if out, err := exec.Command("sshd", "-T").Output(); err == nil {
		if ports := parseSSHDPorts(string(out)); len(ports) > 0 {
			return ports
		}
	}

	// Без прав root sshd -T не работает — читаем конфиги: первый файл с Port выигрывает.
	files, _ := filepath.Glob("/etc/ssh/sshd_config.d/*.conf")
	sort.Strings(files)
	files = append(files, "/etc/ssh/sshd_config")
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if ports := parseSSHDPorts(string(data)); len(ports) > 0 {
			return ports
		}
	}
	return []string{"22"}
}

// parseSSHDPorts понимает и вывод `sshd -T` ("port 22"), и строки конфига ("Port 2222").
func parseSSHDPorts(text string) []string {
	var ports []string
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.EqualFold(f[0], "port") && validatePort(f[1]) == nil && !contains(ports, f[1]) {
			ports = append(ports, f[1])
		}
	}
	return ports
}

func xrdpListenPort() string {
	data, err := os.ReadFile("/etc/xrdp/xrdp.ini")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "port=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "port="))
		}
	}
	return ""
}

func ufwEnabled() bool {
	return fileContains("/etc/ufw/ufw.conf", "ENABLED=yes")
}

func swapActive(path string) bool {
	data, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == path {
			return true
		}
	}
	return false
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
