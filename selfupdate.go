package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

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
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv("GH_TOKEN"))
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

	// Значок на панели запускает копию в /usr/local/bin — обновляем и её.
	if exePath != selfInstallPath && fileExists(selfInstallPath) && os.Geteuid() == 0 {
		if _, err := installSelf(); err != nil {
			fmt.Println(warnStyle.Render("⚠ Не удалось обновить " + selfInstallPath + ": " + err.Error()))
		}
	}
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
