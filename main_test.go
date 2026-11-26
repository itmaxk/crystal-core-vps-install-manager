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

func TestRemovalOrderAndRemovable(t *testing.T) {
	me, _ := user.Current()
	old := stateDir
	stateDir = t.TempDir()
	defer func() { stateDir = old }()
	for _, m := range []string{"xfce", "xrdp", "ai_cli", "update"} {
		_ = os.WriteFile(filepath.Join(stateDir, m+".installed"), nil, 0644)
	}
	_ = os.WriteFile(filepath.Join(stateDir, "create_user.users"), []byte(me.Username+"\n"), 0644)

	got, err := parseRemoveModules("xfce,xrdp,create_user,ai_cli", Config{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ai_cli", "xrdp", "xfce", "create_user"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("порядок удаления %v, ожидалось %v (обратный порядку установки)", got, want)
	}
	for _, id := range []string{"update", "upgrade_ubuntu"} {
		if _, err := parseRemoveModules(id, Config{}); err == nil {
			t.Errorf("модуль %s не должен удаляться", id)
		}
	}
	// Установлено не программой (нет отметки) — не трогаем, даже если компонент есть в системе.
	if _, err := parseRemoveModules("docker", Config{}); err == nil || !strings.Contains(err.Error(), "не устанавливался") {
		t.Errorf("docker без отметки программы не должен удаляться: %v", err)
	}
	all, _ := parseRemoveModules("all", Config{})
	if !reflect.DeepEqual(all, []string{"ai_cli", "xrdp", "xfce", "create_user"}) {
		t.Errorf("--remove all = %v", all)
	}
}

func TestEveryRemovableModuleHasUninstall(t *testing.T) {
	dryRun = true
	defer func() { dryRun = false }()
	old := stateDir
	stateDir = t.TempDir()
	defer func() { stateDir = old }()
	for _, m := range getModuleDefs() {
		cfg := Config{TargetUser: "alice", TargetHome: t.TempDir(), Modules: []string{m.ID}}
		err := uninstallModule(m.ID, &cfg)
		if m.Removable && err != nil {
			t.Errorf("удаление %s в dry-run вернуло ошибку: %v", m.ID, err)
		}
		if !m.Removable && err == nil {
			t.Errorf("для %s удаление должно быть недоступно", m.ID)
		}
	}
}

func TestValidateRemovePlanProtectsAccess(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	oldState, oldGroup := stateDir, groupFile
	stateDir, groupFile = t.TempDir(), filepath.Join(t.TempDir(), "group")
	defer func() { stateDir, groupFile = oldState, oldGroup }()

	_ = os.WriteFile(filepath.Join(stateDir, "create_user.users"), []byte(me.Username+"\nghost-user-xyz\n"), 0644)
	if got := existingCreatedUsers(); !reflect.DeepEqual(got, []string{me.Username}) {
		t.Fatalf("existingCreatedUsers = %v", got)
	}

	cfg := Config{TargetUser: me.Username, Modules: []string{"create_user"}}
	if err := validateRemovePlan(&cfg); err == nil || !strings.Contains(err.Error(), "это вы") {
		t.Errorf("нельзя удалять пользователя, от имени которого запущена программа: %v", err)
	}

	cfg.TargetUser = "someone-else"
	_ = os.WriteFile(groupFile, []byte("sudo:x:27:"+me.Username+"\n"), 0644)
	if err := validateRemovePlan(&cfg); err == nil || !strings.Contains(err.Error(), "администратора") {
		t.Errorf("нельзя удалять последнего администратора: %v", err)
	}

	cfg.Modules = []string{"update"}
	if err := validateRemovePlan(&cfg); err == nil {
		t.Error("update нельзя удалить")
	}
}

func TestRemovalSummaryWarnings(t *testing.T) {
	s := removalSummary([]string{"docker", "ssh_hardening", "xrdp"})
	for _, want := range []string{"Docker", "SSH", "RDP-сессии", "xrdp"} {
		if !strings.Contains(s, want) {
			t.Errorf("в подтверждении нет %q:\n%s", want, s)
		}
	}
}

// Учёт пакетов: apt_install запоминает только новые пакеты, apt_purge_recorded удаляет
// только их и не трогает KEEP_PKGS. apt-get и dpkg-query подменены заглушками.
func TestAptInstallRecordsOnlyNewPackages(t *testing.T) {
	dryRun = false
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0755)
	db := filepath.Join(dir, "installed")
	_ = os.WriteFile(db, []byte("curl\n"), 0644)
	_ = os.WriteFile(filepath.Join(bin, "dpkg-query"), []byte("#!/bin/bash\nif grep -qx \"${@: -1}\" \""+db+"\"; then printf 'install ok installed'; else exit 1; fi\n"), 0755)
	_ = os.WriteFile(filepath.Join(bin, "apt-mark"), []byte("#!/bin/bash\necho \"apt-mark $*\" >> \""+dir+"/apt.log\"\n[ \"$1\" = showauto ] && [ \"$2\" = curl ] && echo curl\nexit 0\n"), 0755)
	_ = os.WriteFile(filepath.Join(bin, "apt-get"), []byte(`#!/bin/bash
echo "apt-get $*" >> "`+dir+`/apt.log"
for a in "$@"; do case "$a" in -*|install|purge|autoremove) ;; *) echo "$a" >> "`+db+`";; esac; done
`), 0755)

	oldLog, oldState := logPath, stateDir
	logPath, stateDir = filepath.Join(dir, "log"), filepath.Join(dir, "state")
	defer func() { logPath, stateDir = oldLog, oldState }()
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	currentModule = "xfce"
	if err := runBash(Config{}, "apt_install curl xfce4 zsh"); err != nil {
		t.Fatalf("apt_install: %v", err)
	}
	rec, _ := os.ReadFile(filepath.Join(stateDir, "xfce.packages"))
	if string(rec) != "xfce4\nzsh\n" {
		t.Errorf("записано %q, ожидались только новые пакеты xfce4, zsh", rec)
	}

	if err := runBash(Config{}, "apt_purge_recorded", "KEEP_PKGS=zsh"); err != nil {
		t.Fatalf("apt_purge_recorded: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "apt.log"))
	if !strings.Contains(string(log), "purge xfce4\n") {
		t.Errorf("ожидался purge только xfce4, лог:\n%s", log)
	}
	if !strings.Contains(string(log), "apt-mark auto curl") {
		t.Errorf("уже стоявшая автоматическая зависимость должна остаться «auto»:\n%s", log)
	}
	if strings.Contains(string(log), "purge curl") || strings.Contains(string(log), "purge xfce4 zsh") {
		t.Errorf("удалены лишние пакеты:\n%s", log)
	}
	if fileExists(filepath.Join(stateDir, "xfce.packages")) {
		t.Error("после удаления учёт пакетов должен быть очищен")
	}
}

func TestRemovePluginFromPanelXML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xfce4-panel.xml")
	xml := `<channel name="xfce4-panel" version="1.0">
  <property name="panels" type="array">
    <value type="int" value="1"/>
    <value type="int" value="26"/>
    <property name="panel-2" type="empty">
      <property name="plugin-ids" type="array">
        <value type="int" value="20"/>
        <value type="int" value="26"/>
        <value type="int" value="21"/>
      </property>
    </property>
  </property>
</channel>`
	_ = os.WriteFile(path, []byte(xml), 0644)
	if err := removePluginFromPanelXML(path, "26"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if strings.Count(string(got), `value="26"`) != 1 {
		t.Errorf("26 должен остаться только в списке panels:\n%s", got)
	}
	if !strings.Contains(string(got), "<value type=\"int\" value=\"20\"/>\n        <value type=\"int\" value=\"21\"/>") {
		t.Errorf("plugin-ids испорчен:\n%s", got)
	}
}

func TestPanelScriptSyntax(t *testing.T) {
	for _, f := range []string{"configs/vps-setup-panel.sh", "configs/vps-setup-launch.sh"} {
		if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", f, err, out)
		}
	}
}

func TestSysInfoParsers(t *testing.T) {
	if got := parseOSRelease("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"); got != "Ubuntu 24.04.1 LTS" {
		t.Errorf("parseOSRelease = %q", got)
	}
	if got := parseCPUModel("processor : 0\nmodel name\t: Intel(R) Xeon(R)   CPU @ 2.20GHz\n", ""); got != "Intel(R) Xeon(R) CPU @ 2.20GHz" {
		t.Errorf("x86 cpu = %q", got)
	}
	if got := parseCPUModel("processor : 0\nBogoMIPS : 50.00\n", "Architecture: aarch64\nVendor ID: ARM\nModel name: Neoverse-N1\n"); got != "ARM Neoverse-N1" {
		t.Errorf("arm cpu = %q", got)
	}
	total, avail, swap := parseMeminfo("MemTotal:       24563440 kB\nMemFree: 1 kB\nMemAvailable:   20000000 kB\nSwapTotal:       2097148 kB\n")
	if total != 24563440*1024 || avail != 20000000*1024 || swap != 2097148*1024 {
		t.Errorf("meminfo = %d %d %d", total, avail, swap)
	}
	// util-linux 2.37 (Ubuntu 22.04) и более новые/старые форматы JSON
	disks := parseLsblk([]byte(`{"blockdevices":[{"name":"sda","size":214748364800,"type":"disk","model":"BlockVolume","rota":false},
		{"name":"loop0","size":"1000","type":"loop","model":null,"rota":"0"},
		{"name":"vdb","size":"53687091200","type":"disk","model":null,"rota":"1"}]}`))
	if len(disks) != 2 || disks[0].Name != "sda" || disks[0].Size != 214748364800 || disks[0].Rotational ||
		disks[1].Name != "vdb" || !disks[1].Rotational || disks[1].Model != "" {
		t.Errorf("parseLsblk = %+v", disks)
	}
	if got := humanBytes(214748364800); got != "200.0 ГБ" {
		t.Errorf("humanBytes = %q", got)
	}
	for n, want := range map[int]string{1: "ядро", 2: "ядра", 4: "ядра", 5: "ядер", 11: "ядер", 12: "ядер", 21: "ядро", 22: "ядра"} {
		if got := pluralRu(n, "ядро", "ядра", "ядер"); got != want {
			t.Errorf("pluralRu(%d) = %q, want %q", n, got, want)
		}
	}
	out := formatSysInfo(collectSysInfo())
	for _, want := range []string{"ОС", "Процессор", "Память", "Диски"} {
		if !strings.Contains(out, want) {
			t.Errorf("в сведениях о системе нет %q:\n%s", want, out)
		}
	}
}

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.1.0", "v1.0.1", true},
		{"v1.2.0", "v1.10.0", false},
		{"v1.10.0", "v1.9.3", true},
		{"v2.0.0", "v1.99.99", true},
		{"v1.1.0", "v1.1.0", false},
		{"v1.0.1", "v1.1.0", false},
		{"v1.1.0", "v1.1.0-3-gabc123-dirty", false},
		{"v1.2.0", "v1.1.0-3-gabc123", true},
		{"v1.1.0", "dev", true},
		{"v1.1.0", "7707d9e-dirty", true},
		{"nightly", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := isNewerVersion(c.latest, c.current); got != c.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
