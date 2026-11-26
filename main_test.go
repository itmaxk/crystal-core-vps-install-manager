package main

import (
	"os"
	"path/filepath"
	"reflect"
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

	if err := runBash("exit 1"); err != nil {
		t.Errorf("в режиме dry-run runBash не должен выполнять команду, но вернул: %v", err)
	}
}
