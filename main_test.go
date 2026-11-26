package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseOnlyModules(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name:  "валидный список",
			input: "xfce,xrdp,docker",
			want:  []string{"xfce", "xrdp", "docker"},
		},
		{
			name:  "пробелы и дубликаты",
			input: " xfce , xrdp, xfce , , docker ",
			want:  []string{"xfce", "xrdp", "docker"},
		},
		{
			name:  "порядок выполнения не зависит от порядка в --only",
			input: "upgrade_ubuntu,docker,xrdp,create_user,xfce",
			want:  []string{"create_user", "xfce", "xrdp", "docker", "upgrade_ubuntu"},
		},
		{
			name:    "неизвестный модуль",
			input:   "xfce,unknown_mod",
			wantErr: true,
		},
		{
			name:    "пустой список после запятых",
			input:   " , , ",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOnlyModules(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseOnlyModules(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseOnlyModules(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestWriteEmbeddedFile(t *testing.T) {
	dryRun = false
	dest := filepath.Join(t.TempDir(), "kitty", "kitty.conf")

	if err := writeEmbeddedFile("configs/kitty.conf", dest, 0644); err != nil {
		t.Fatalf("writeEmbeddedFile failed: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("не удалось прочитать созданный файл: %v", err)
	}
	if !strings.Contains(string(data), "JetBrainsMono Nerd Font") {
		t.Errorf("ожидался шрифт JetBrainsMono Nerd Font в kitty.conf, получено:\n%s", string(data))
	}
}

func TestWriteEmbeddedTemplate(t *testing.T) {
	dryRun = false
	dest := filepath.Join(t.TempDir(), "fail2ban", "jail.local")
	cfg := Config{SSHPort: "2299"}

	if err := writeEmbeddedTemplate("configs/jail.local.tmpl", dest, cfg, 0644); err != nil {
		t.Fatalf("writeEmbeddedTemplate failed: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("не удалось прочитать отрендеренный шаблон: %v", err)
	}
	if !strings.Contains(string(data), "port    = 2299") {
		t.Errorf("порт SSH не подставился в jail.local:\n%s", string(data))
	}
}

func TestAppendEmbeddedBlockIdempotent(t *testing.T) {
	dryRun = false
	dest := filepath.Join(t.TempDir(), ".zshrc")
	marker := "# >>> vps-setup zsh >>>"

	if err := appendEmbeddedBlock("configs/zshrc.snippet", dest, marker); err != nil {
		t.Fatalf("первый appendEmbeddedBlock failed: %v", err)
	}
	if err := appendEmbeddedBlock("configs/zshrc.snippet", dest, marker); err != nil {
		t.Fatalf("второй appendEmbeddedBlock failed: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("не удалось прочитать .zshrc: %v", err)
	}

	count := strings.Count(string(data), marker)
	if count != 1 {
		t.Errorf("ожидалось ровно 1 вхождение маркера, получено %d", count)
	}
}

func TestDryRunDoesNotModifySystem(t *testing.T) {
	dryRun = true
	defer func() { dryRun = false }()

	tmpDir := t.TempDir()
	fileDest := filepath.Join(tmpDir, "kitty.conf")
	tmplDest := filepath.Join(tmpDir, "jail.local")
	zshDest := filepath.Join(tmpDir, ".zshrc")

	if err := writeEmbeddedFile("configs/kitty.conf", fileDest, 0644); err != nil {
		t.Fatalf("dry-run writeEmbeddedFile вернул ошибку: %v", err)
	}
	if err := writeEmbeddedTemplate("configs/jail.local.tmpl", tmplDest, Config{SSHPort: "2222"}, 0644); err != nil {
		t.Fatalf("dry-run writeEmbeddedTemplate вернул ошибку: %v", err)
	}
	if err := appendEmbeddedBlock("configs/zshrc.snippet", zshDest, "# >>> vps-setup zsh >>>"); err != nil {
		t.Fatalf("dry-run appendEmbeddedBlock вернул ошибку: %v", err)
	}

	for _, p := range []string{fileDest, tmplDest, zshDest} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("в режиме dry-run файл %s не должен создаваться", p)
		}
	}

	if err := runBash(Config{}, "exit 1"); err != nil {
		t.Errorf("в режиме dry-run runBash не должен выполнять команду, но вернул: %v", err)
	}
}

func TestDesktopAndRDPRunFirst(t *testing.T) {
	order := orderModules([]string{"ai_cli", "fail2ban_ufw", "ssh_hardening", "update", "xrdp", "zsh_starship", "xfce", "upgrade_ubuntu"})
	pos := map[string]int{}
	for i, m := range order {
		pos[m] = i
	}
	for _, later := range []string{"ai_cli", "fail2ban_ufw", "ssh_hardening", "zsh_starship", "upgrade_ubuntu"} {
		if pos["xrdp"] > pos[later] {
			t.Errorf("xrdp должен выполняться раньше %s, порядок: %v", later, order)
		}
	}
	if pos["xfce"] > pos["xrdp"] {
		t.Errorf("xfce должен выполняться раньше xrdp: %v", order)
	}
	if order[len(order)-1] != "upgrade_ubuntu" {
		t.Errorf("upgrade_ubuntu должен быть последним: %v", order)
	}
	if pos["ssh_hardening"] > pos["fail2ban_ufw"] {
		t.Errorf("ssh_hardening должен идти до fail2ban_ufw (UFW открывает уже новый порт): %v", order)
	}
}

func TestPresetRDP(t *testing.T) {
	got, err := presetModules(presetRDP, Config{TargetUser: "root", TargetHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if !contains(rdpPresetModules, m) {
			t.Errorf("модуль %s не входит в быстрый старт", m)
		}
	}
	if !contains(got, "create_user") {
		t.Errorf("под root быстрый старт должен создавать пользователя: %v", got)
	}
	if _, err := presetModules("bogus", Config{}); err == nil {
		t.Error("ожидалась ошибка для неизвестного пресета")
	}
}

func TestEveryModuleHasImplementation(t *testing.T) {
	dryRun = true
	defer func() { dryRun = false }()
	for _, m := range getModuleDefs() {
		cfg := Config{TargetUser: "alice", TargetHome: t.TempDir(), SSHPort: "22", NewUsername: "bob", Password: "secret123", Modules: []string{m.ID}}
		if err := executeModule(m.ID, &cfg); err != nil && !strings.Contains(err.Error(), "root") {
			t.Errorf("модуль %s в dry-run вернул ошибку: %v", m.ID, err)
		}
	}
	if err := executeModule("nope", &Config{}); err == nil {
		t.Error("ожидалась ошибка для неизвестного модуля")
	}
}

const sampleXrdpIni = `[Globals]
ini_version=1
port=3389
use_vsock=false

[Xorg]
name=Xorg
lib=libxup.so
username=ask
password=ask
ip=127.0.0.1
port=-1
code=20

[Xvnc]
name=Xvnc
ip=127.0.0.1
port=-1

[vnc-any]
port=ask5900
`

func TestXrdpIniEditOnlyChangesGlobalsPort(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash недоступен")
	}
	ini := filepath.Join(t.TempDir(), "xrdp.ini")
	// Имитируем файл, испорченный старой версией: порт сессии Xorg перезаписан.
	broken := strings.Replace(sampleXrdpIni, "ip=127.0.0.1\nport=-1\ncode=20", "ip=127.0.0.1\nport=tcp://127.0.0.1:3389\ncode=20", 1)
	if err := os.WriteFile(ini, []byte(broken), 0644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // повторный запуск должен давать тот же результат
		cmd := exec.Command("bash", "-euo", "pipefail", "-c", xrdpIniEdit)
		cmd.Env = append(os.Environ(), "XRDP_INI="+ini, "XRDP_PORT=tcp://127.0.0.1:3389")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("xrdpIniEdit: %v\n%s", err, out)
		}
	}

	data, _ := os.ReadFile(ini)
	got := string(data)
	want := strings.Replace(sampleXrdpIni, "port=3389", "port=tcp://127.0.0.1:3389", 1)
	if got != want {
		t.Errorf("неожиданный xrdp.ini:\n%s\nожидалось:\n%s", got, want)
	}
}

func TestExtractKeysStripsCloudImageRootOptions(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash недоступен")
	}
	ak := filepath.Join(t.TempDir(), "authorized_keys")
	content := `no-port-forwarding,no-agent-forwarding,no-X11-forwarding,command="echo 'Please login as the user \"ubuntu\" rather than the user \"root\".';echo;sleep 10;exit 142" ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7 user@laptop
# comment line
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMq work key
sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29t yubikey
garbage line
`
	if err := os.WriteFile(ak, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", extractKeysFn+`extract_keys "$1"`, "bash", ak)
	cmd.Env = append(os.Environ(), "KEY_RE="+sshKeyRegex)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("extract_keys: %v", err)
	}
	want := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7 user@laptop\n" +
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMq work key\n" +
		"sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29t yubikey\n"
	if string(out) != want {
		t.Errorf("extract_keys вывел:\n%s\nожидалось:\n%s", out, want)
	}
	if !hasValidKeyFile(ak) {
		t.Error("hasValidKeyFile должен найти ключ")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	_ = os.WriteFile(empty, []byte("# no keys\n\n"), 0600)
	if hasValidKeyFile(empty) {
		t.Error("файл без ключей не должен считаться валидным")
	}
}

func TestParseSSHDPorts(t *testing.T) {
	got := parseSSHDPorts("port 22\naddressfamily any\nport 2222\nport 22\n")
	if !reflect.DeepEqual(got, []string{"22", "2222"}) {
		t.Errorf("sshd -T: %v", got)
	}
	got = parseSSHDPorts("# Port 22\nPort 2200\nPasswordAuthentication no\nPort abc\n")
	if !reflect.DeepEqual(got, []string{"2200"}) {
		t.Errorf("config: %v", got)
	}
}

func TestValidatePlan(t *testing.T) {
	home := t.TempDir()
	base := Config{TargetUser: "root", TargetHome: home, SSHPort: "22"}

	cfg := base
	cfg.Modules = []string{"ssh_hardening"}
	if err := validatePlan(&cfg); err == nil || !strings.Contains(err.Error(), "root") {
		t.Errorf("ssh_hardening под root без create_user должен быть запрещён, получено: %v", err)
	}

	cfg = base
	cfg.Modules = []string{"create_user", "xfce"}
	if err := validatePlan(&cfg); err == nil {
		t.Error("create_user без имени должен быть запрещён")
	}

	cfg = base
	cfg.Modules = []string{"create_user"}
	cfg.NewUsername = "dev"
	cfg.Password = "short"
	if err := validatePlan(&cfg); err == nil {
		t.Error("create_user с коротким паролем должен быть запрещён")
	}

	cfg = base
	cfg.Modules = []string{"create_user"}
	cfg.NewUsername = "dev; rm -rf /"
	cfg.Password = "longenough"
	if err := validatePlan(&cfg); err == nil {
		t.Error("имя пользователя с шелл-символами должно быть отклонено")
	}

	cfg = base
	cfg.TargetUser = "alice"
	cfg.Modules = []string{"ssh_hardening"}
	if err := validatePlan(&cfg); err == nil {
		t.Error("ssh_hardening без authorized_keys должен быть запрещён")
	}
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0700)
	_ = os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMq me\n"), 0600)
	if err := validatePlan(&cfg); err != nil {
		t.Errorf("валидный план отклонён: %v", err)
	}

	cfg.SSHPort = "70000"
	if err := validatePlan(&cfg); err == nil {
		t.Error("порт 70000 должен быть отклонён")
	}
}

func TestRunBashStrictModeAndEnv(t *testing.T) {
	dryRun = false
	oldLog := logPath
	logPath = filepath.Join(t.TempDir(), "vps-setup.log")
	defer func() { logPath = oldLog }()

	cfg := Config{TargetUser: "alice", TargetHome: "/home/alice"}
	if err := runBash(cfg, `echo "user=$TARGET_USER home=$TARGET_HOME extra=$EXTRA"`, "EXTRA=42"); err != nil {
		t.Fatalf("runBash: %v", err)
	}
	if err := runBash(cfg, "false\necho unreachable"); err == nil {
		t.Error("set -e: ошибка команды должна прерывать скрипт")
	}
	if err := runBash(cfg, "echo $UNDEFINED_VAR_XYZ"); err == nil {
		t.Error("set -u: неопределённая переменная должна давать ошибку")
	}
	if err := runBash(cfg, "false | true"); err == nil {
		t.Error("pipefail: ошибка в пайпе должна давать ошибку")
	}
	if err := runBash(cfg, `type apt-get | grep -q function && type as_user >/dev/null && type fix_user_dirs >/dev/null`); err != nil {
		t.Errorf("преамбула не подключилась: %v", err)
	}

	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "user=alice home=/home/alice extra=42") {
		t.Errorf("в логе нет вывода скрипта:\n%s", data)
	}
	if strings.Contains(string(data), "unreachable") {
		t.Error("скрипт продолжился после ошибки")
	}
}

func TestMkdirAllOwned(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := mkdirAllOwned(dir, uid, gid); err != nil {
		t.Fatalf("mkdirAllOwned: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("каталог не создан: %v", err)
	}
	if err := mkdirAllOwned(dir, uid, gid); err != nil {
		t.Errorf("повторный вызов: %v", err)
	}
}

func TestFindChecksum(t *testing.T) {
	sums := "abc123  vps-setup-linux-amd64\nDEF456 *vps-setup-linux-arm64\n"
	if got := findChecksum(sums, "vps-setup-linux-arm64"); got != "def456" {
		t.Errorf("findChecksum = %q", got)
	}
	if got := findChecksum(sums, "vps-setup-linux-riscv64"); got != "" {
		t.Errorf("findChecksum для отсутствующего файла = %q", got)
	}
}
