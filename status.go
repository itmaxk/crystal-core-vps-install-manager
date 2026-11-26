package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
)

// --- Статус и мониторинг ---

type ModuleStatus struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Ready   bool   `json:"ready"`
	Details string `json:"details,omitempty"`
}

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
	case "panel_launcher":
		if fileExists(desktopFilePath) {
			return "меню «Система» + нижняя панель"
		}
	case "ollama":
		if serviceActive("ollama") {
			return "127.0.0.1:11434"
		}
	}
	return ""
}
