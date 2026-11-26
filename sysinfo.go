package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/charmbracelet/lipgloss"
)

type DiskInfo struct {
	Name       string
	Size       uint64
	Model      string
	Rotational bool
}

type SysInfo struct {
	OS           string
	Kernel       string
	Arch         string
	Hostname     string
	CPU          string
	Cores        int
	MemTotal     uint64
	MemAvailable uint64
	SwapTotal    uint64
	Disks        []DiskInfo
	RootTotal    uint64
	RootFree     uint64
}

func collectSysInfo() SysInfo {
	si := SysInfo{Arch: runtime.GOARCH, Cores: runtime.NumCPU()}

	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		si.OS = parseOSRelease(string(data))
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		si.Kernel = strings.TrimSpace(string(data))
	}
	si.Hostname, _ = os.Hostname()

	cpuinfo, _ := os.ReadFile("/proc/cpuinfo")
	si.CPU = parseCPUModel(string(cpuinfo), "")
	if si.CPU == "" {
		if out, err := exec.Command("lscpu").Output(); err == nil {
			si.CPU = parseCPUModel("", string(out))
		}
	}

	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		si.MemTotal, si.MemAvailable, si.SwapTotal = parseMeminfo(string(data))
	}

	if out, err := exec.Command("lsblk", "-J", "-b", "-d", "-o", "NAME,SIZE,TYPE,MODEL,ROTA").Output(); err == nil {
		si.Disks = parseLsblk(out)
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		si.RootTotal = st.Blocks * uint64(st.Bsize)
		si.RootFree = st.Bavail * uint64(st.Bsize)
	}
	return si
}

func parseOSRelease(text string) string {
	values := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[k] = strings.Trim(v, `"'`)
		}
	}
	if v := values["PRETTY_NAME"]; v != "" {
		return v
	}
	return strings.TrimSpace(values["NAME"] + " " + values["VERSION"])
}

// parseCPUModel берёт «model name» из /proc/cpuinfo (x86) или «Model name» из lscpu (ARM).
func parseCPUModel(cpuinfo, lscpu string) string {
	for _, line := range strings.Split(cpuinfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "model name" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	var vendor, model string
	for _, line := range strings.Split(lscpu, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Vendor ID":
			if vendor == "" {
				vendor = strings.TrimSpace(v)
			}
		case "Model name":
			if model == "" {
				model = strings.TrimSpace(v)
			}
		}
	}
	if model != "" && vendor != "" && !strings.Contains(strings.ToLower(model), strings.ToLower(vendor)) {
		return vendor + " " + model
	}
	return model
}

func parseMeminfo(text string) (total, available, swap uint64) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		case "SwapTotal:":
			swap = kb * 1024
		}
	}
	return
}

// parseLsblk разбирает `lsblk -J -b -d`; в разных версиях util-linux числа и флаги
// приходят то числами/булевыми, то строками.
func parseLsblk(data []byte) []DiskInfo {
	var parsed struct {
		Blockdevices []map[string]any `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	var disks []DiskInfo
	for _, d := range parsed.Blockdevices {
		if str(d["type"]) != "disk" {
			continue
		}
		size, _ := strconv.ParseUint(str(d["size"]), 10, 64)
		rota := str(d["rota"])
		disks = append(disks, DiskInfo{
			Name:       str(d["name"]),
			Size:       size,
			Model:      strings.TrimSpace(str(d["model"])),
			Rotational: rota == "true" || rota == "1",
		})
	}
	return disks
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', 0, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprint(t)
	}
}

func humanBytes(b uint64) string {
	const unit = 1024.0
	v := float64(b)
	switch {
	case v >= unit*unit*unit*unit:
		return fmt.Sprintf("%.1f ТБ", v/(unit*unit*unit*unit))
	case v >= unit*unit*unit:
		return fmt.Sprintf("%.1f ГБ", v/(unit*unit*unit))
	case v >= unit*unit:
		return fmt.Sprintf("%.0f МБ", v/(unit*unit))
	default:
		return fmt.Sprintf("%d Б", b)
	}
}

// pluralRu: 1 ядро, 2 ядра, 5 ядер.
func pluralRu(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	default:
		return many
	}
}

func formatSysInfo(si SysInfo) string {
	label := lipgloss.NewStyle().Foreground(lipgloss.Color("#7D56F4")).Bold(true).Width(11)
	value := lipgloss.NewStyle().Foreground(lipgloss.Color("#DDDDDD"))
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))

	row := func(name, val string) string {
		return label.Render(name) + value.Render(val)
	}

	osLine := si.OS
	if osLine == "" {
		osLine = "неизвестна"
	}
	osLine += dim.Render(fmt.Sprintf("  ядро %s, %s, хост %s", si.Kernel, si.Arch, si.Hostname))

	cpu := si.CPU
	if cpu == "" {
		cpu = "неизвестен"
	}
	cpu += fmt.Sprintf(" — %d %s", si.Cores, pluralRu(si.Cores, "ядро", "ядра", "ядер"))

	mem := fmt.Sprintf("%s, доступно %s", humanBytes(si.MemTotal), humanBytes(si.MemAvailable))
	if si.SwapTotal > 0 {
		mem += dim.Render(" · swap " + humanBytes(si.SwapTotal))
	} else {
		mem += dim.Render(" · swap нет")
	}

	var diskLines []string
	for _, d := range si.Disks {
		kind := "SSD"
		if d.Rotational {
			kind = "HDD"
		}
		line := fmt.Sprintf("%s — %s, %s", d.Name, humanBytes(d.Size), kind)
		if d.Model != "" {
			line += ", " + d.Model
		}
		diskLines = append(diskLines, line)
	}
	if si.RootTotal > 0 {
		used := si.RootTotal - si.RootFree
		pct := float64(used) * 100 / float64(si.RootTotal)
		diskLines = append(diskLines, dim.Render(fmt.Sprintf("/ занято %s из %s (%.0f%%), свободно %s",
			humanBytes(used), humanBytes(si.RootTotal), pct, humanBytes(si.RootFree))))
	}
	if len(diskLines) == 0 {
		diskLines = []string{"нет данных"}
	}

	lines := []string{
		row("ОС", osLine),
		row("Процессор", cpu),
		row("Память", mem),
		row("Диски", diskLines[0]),
	}
	for _, l := range diskLines[1:] {
		lines = append(lines, label.Render("")+value.Render(l))
	}
	return strings.Join(lines, "\n")
}

func printSysInfo(si SysInfo) {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#44475A")).
		Padding(0, 2).
		MarginBottom(1)
	fmt.Println(box.Render(formatSysInfo(si)))
}
