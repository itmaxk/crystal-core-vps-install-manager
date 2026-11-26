package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

//go:embed configs/*
var configFS embed.FS

//go:embed assets/icon.png
var iconPNG []byte

var (
	Version = "dev"
	dryRun  bool

	// Пути — переменные (а не константы), чтобы тесты могли подменить их на временные.
	logPath   = "/var/log/vps-setup.log"
	stateDir  = "/var/lib/vps-setup"
	groupFile = "/etc/group"

	// currentModule передаётся скриптам как $MODULE (для учёта установленных пакетов).
	currentModule string

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4")).MarginBottom(1)
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")).Bold(true)
	errStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F87")).Bold(true)
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C"))
	dryStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD"))
	hintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Bold(true)
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
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

type Config struct {
	TargetUser  string
	TargetHome  string
	SourceHome  string // домашний каталог пользователя, запустившего sudo (откуда копировать SSH-ключи)
	SSHPort     string // порт SSH, который выставит ssh_hardening
	NewUsername string
	Password    string // пароль нового пользователя или пароль для RDP-входа текущего
	Modules     []string
}

func main() {
	showVersion := flag.Bool("version", false, "Показать текущую версию")
	doUpdate := flag.Bool("self-update", false, "Обновить бинарник из GitHub Releases")
	showStatus := flag.Bool("status", false, "Показать состояние модулей")
	asJSON := flag.Bool("json", false, "Вывести статус в формате JSON (используется с --status)")
	onlyFlag := flag.String("only", "", "Неинтерактивная установка указанных модулей через запятую (например: xfce,xrdp,docker)")
	presetFlag := flag.String("preset", "", "Неинтерактивная установка набора модулей: rdp (рабочий стол + RDP) или full (все рекомендуемые)")
	removeFlag := flag.String("remove", "", "Неинтерактивное удаление модулей через запятую или всего установленного программой: --remove all")
	userFlag := flag.String("user", "", "Целевой пользователь (для запуска под root из cloud-init / Ansible)")
	newUserFlag := flag.String("new-user", "", "Имя нового sudo-пользователя для create_user в неинтерактивном режиме (пароль — в переменной "+passwordEnv+")")
	sshPortFlag := flag.String("ssh-port", "", "Порт SSH для ssh_hardening (по умолчанию — текущий порт sshd, без смены)")
	autoReboot := flag.Bool("auto-reboot", false, "Автоматически перезагрузить сервер в конце, если система этого требует")
	flag.BoolVar(&dryRun, "dry-run", false, "Показать команды и файлы без внесения изменений в систему")
	flag.Parse()

	quiet := *showStatus && *asJSON
	if !quiet {
		printBanner()
	}

	if *showVersion {
		fmt.Printf("Crystal-Core VPS Install Manager %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return
	}
	if !quiet {
		printSysInfo(collectSysInfo())
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
		fatal("Запустите программу через sudo (sudo vps-setup)")
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

	switch {
	case *removeFlag != "":
		if *onlyFlag != "" || *presetFlag != "" {
			fatal("--remove нельзя сочетать с --only и --preset")
		}
		mods, err := parseRemoveModules(*removeFlag, cfg)
		if err != nil {
			fatal(err.Error())
		}
		if len(mods) == 0 {
			fmt.Println(okStyle.Render("✓ Удалять нечего: программа ничего не устанавливала или всё уже удалено."))
			return
		}
		cfg.Modules = mods
		if err := validateRemovePlan(&cfg); err != nil {
			fatal(err.Error())
		}
		runRemove(cfg)

	case *onlyFlag != "" || *presetFlag != "":
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
		if err := validatePlan(&cfg); err != nil {
			fatal(err.Error())
		}
		runInstall(cfg, *autoReboot)

	default:
		interactiveLoop(cfg, *autoReboot)
	}
}

func fatal(msg string) {
	fmt.Println(errStyle.Render("✖ " + msg))
	os.Exit(1)
}

// runInstall выполняет модули по порядку; упавший модуль не останавливает остальные
// (кроме create_user — от него зависят все пользовательские настройки).
func runInstall(cfg Config, autoReboot bool) {
	fmt.Println(titleStyle.Render("Установка: " + strings.Join(cfg.Modules, " → ")))

	appendLog(fmt.Sprintf("\n=== Crystal-Core VPS Install Manager %s — установка — %s ===\nМодули: %s\n",
		Version, time.Now().Format(time.RFC3339), strings.Join(cfg.Modules, ",")))

	if installed, err := installSelf(); err != nil {
		fmt.Println(warnStyle.Render("⚠ Не удалось скопировать программу в " + selfInstallPath + ": " + err.Error()))
	} else if installed {
		fmt.Println(dimStyle.Render("Программа установлена в " + selfInstallPath + " — дальше можно запускать просто: sudo vps-setup"))
	}

	if needsApt(cfg.Modules) {
		currentModule = ""
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
		markInstalled(mod)
		recordModuleUser(mod, cfg.TargetUser)
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
	if contains(cfg.Modules, "panel_launcher") && !contains(failed, "panel_launcher") {
		fmt.Println(dimStyle.Render("Значок программы — на нижней панели XFCE и в меню «Приложения → Система» (в открытых сессиях появится сразу, в новых — при входе)."))
	}
	if contains(cfg.Modules, "tailscale") && !contains(failed, "tailscale") {
		fmt.Println(warnStyle.Render("Для активации Tailscale выполните: sudo tailscale up"))
	}
	if !dryRun && cfg.TargetUser != "root" && contains(cfg.Modules, "xrdp") && !userHasPassword(cfg.TargetUser) {
		fmt.Println(warnStyle.Render(fmt.Sprintf("⚠ У пользователя %s не задан пароль — войти по RDP не получится. Задайте его: sudo passwd %s", cfg.TargetUser, cfg.TargetUser)))
	}

	handleReboot(cfg, autoReboot)

	if len(failed) > 0 {
		os.Exit(1)
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
		Render("Crystal-Core VPS Install Manager " + Version)
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
		Padding(0, 2)

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
		fmt.Println(warnStyle.Render("  [DRY-RUN] " + err.Error() + " (в реальном запуске операция не начнётся)"))
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

// isAbort — пользователь нажал Esc / Ctrl+C в форме.
func isAbort(err error) bool {
	return errors.Is(err, huh.ErrUserAborted)
}
