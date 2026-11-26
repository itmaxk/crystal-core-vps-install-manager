package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// bashPreamble добавляется к каждому скрипту:
//   - apt-get всегда неинтерактивен и ждёт освобождения dpkg-lock (на свежем VPS
//     первые минуты работает unattended-upgrades), needrestart не задаёт вопросов;
//   - apt_install ставит пакеты и запоминает в $STATE_DIR/$MODULE.packages только те,
//     которых раньше не было, — именно их потом удалит apt_purge_recorded;
//   - as_user выполняет команду от имени целевого пользователя в его HOME;
//   - fix_user_dirs чинит владельца ~/.config и т.п., если их когда-то создал root.
const bashPreamble = `
export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a
mkdir -p "$STATE_DIR" 2>/dev/null || true

apt-get() {
	command apt-get -y -o DPkg::Lock::Timeout=600 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "$@"
}
is_installed() {
	[ "$(dpkg-query -W -f='${Status}' "$1" 2>/dev/null || true)" = "install ok installed" ]
}
apt_install() {
	local p
	local -a new=() auto=()
	for p in "$@"; do
		if ! is_installed "$p"; then
			new+=("$p")
		elif [ "$(apt-mark showauto "$p" 2>/dev/null || true)" = "$p" ]; then
			auto+=("$p")
		fi
	done
	apt-get install "$@"
	# apt-get install помечает уже стоявшую зависимость «установленной вручную» —
	# возвращаем пометку, иначе autoremove её потом не уберёт.
	if [ "${#auto[@]}" -gt 0 ]; then
		apt-mark auto "${auto[@]}" >/dev/null
	fi
	if [ "${#new[@]}" -gt 0 ]; then
		printf '%s\n' "${new[@]}" >> "$STATE_DIR/$MODULE.packages"
	fi
}
# apt_purge_recorded — удаляет только пакеты, которые поставил этот модуль (учёт в
# $STATE_DIR/$MODULE.packages). Пакеты из $KEEP_PKGS (по одному в строке) не трогает.
apt_purge_recorded() {
	local f="$STATE_DIR/$MODULE.packages" p
	local -a pkgs=() purge=()
	if [ -f "$f" ]; then
		mapfile -t pkgs < <(sort -u "$f")
	fi
	for p in "${pkgs[@]}"; do
		if [ -n "$p" ] && is_installed "$p" && ! grep -qxF -- "$p" <<<"${KEEP_PKGS:-}"; then
			purge+=("$p")
		fi
	done
	if [ "${#purge[@]}" -gt 0 ]; then
		apt-get purge "${purge[@]}"
		apt-get autoremove --purge
	fi
	rm -f "$f"
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
	cmd.Env = append(os.Environ(),
		"TARGET_USER="+cfg.TargetUser,
		"TARGET_HOME="+cfg.TargetHome,
		"STATE_DIR="+stateDir,
		"MODULE="+currentModule,
	)
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

// --- Запись файлов ---

func writeEmbeddedFile(embedPath, destPath string, perm os.FileMode) error {
	data, err := configFS.ReadFile(embedPath)
	if err != nil {
		return err
	}
	return writeBytesFile(destPath, data, perm, embedPath)
}

func writeBytesFile(destPath string, data []byte, perm os.FileMode, source string) error {
	if dryRun {
		fmt.Println(dryStyle.Render(fmt.Sprintf("  [DRY-RUN] Запись файла: %s (из %s, права %04o)", destPath, source, perm)))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(destPath, data, perm); err != nil {
		return err
	}
	return os.Chmod(destPath, perm)
}

// removeIfEmbeddedEqual удаляет файл, только если он не менялся после того, как его
// записала программа (содержимое совпадает со встроенным).
func removeIfEmbeddedEqual(embedPath, destPath string) error {
	want, err := configFS.ReadFile(embedPath)
	if err != nil {
		return err
	}
	got, err := os.ReadFile(destPath)
	if err != nil || !bytes.Equal(got, want) {
		return nil
	}
	if dryRun {
		fmt.Println(dryStyle.Render("  [DRY-RUN] Удаление файла: " + destPath))
		return nil
	}
	return os.Remove(destPath)
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
