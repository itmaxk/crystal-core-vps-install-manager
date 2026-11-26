# Crystal-Core VPS Install Manager

<p align="center">
  <img src="assets/icon.jpeg" alt="Crystal-Core VPS Install Manager" width="180" />
</p>

**Crystal-Core VPS Install Manager** (`vps-setup`) — интерактивный TUI- и CLI-установщик на Go (Charmbracelet) для быстрой, безопасной и воспроизводимой настройки Ubuntu VPS под ключ: от обновления ОС и харденинга SSH до графической среды XFCE + XRDP, Docker и AI CLI-инструментов.

> **Crystal-Core — Автоматизация бизнеса и IT-инфраструктура под ключ**  
> Заказная разработка, AI-агенты, интеграции и настройка серверов.  
> Сайт: [crystal-core.com](https://crystal-core.com/) | Почта: [info@crystal-core.com](mailto:info@crystal-core.com)

---

## Возможности

* **Один статический бинарник:** без внешних зависимостей, все конфиги вшиты через `//go:embed`.
* **Умное TUI-меню:** автоматически определяет уже установленные модули, помечает их `[установлено]` и снимает с них галочки при повторном запуске.
* **Безопасность по умолчанию:** создание `sudo`-пользователя с переносом SSH-ключей, харденинг SSH с защитой от блокировки, Fail2Ban, UFW и RDP строго через SSH-туннель или Tailscale.
* **Автоматизация и CI/CD:** неинтерактивный режим (`--only`, `--user`, `--auto-reboot`), пробный прогон (`--dry-run`) и мониторинг (`--status --json`).
* **Самообновление:** обновление бинарника из GitHub Releases одной командой (`--self-update`).

---

## Модули установки

### Система, безопасность и сеть
| ID | Модуль | Описание | По умолч. |
|---|---|---|---|
| `upgrade_ubuntu` | Обновление Ubuntu | `full-upgrade`, переход на последнюю LTS-версию (`do-release-upgrade`), проверка перезагрузки | Вкл |
| `create_user` | Создать sudo-пользователя | Новый аккаунт с паролем для `sudo`/XRDP и перенос SSH-ключей из `/root/.ssh/authorized_keys` | Вкл (под root) |
| `update` | Базовые CLI-утилиты | `curl`, `wget`, `git`, `htop`, `btop`, `tmux`, `unzip`, `net-tools`, `fontconfig`, `gnupg` | Вкл |
| `ssh_hardening` | Харденинг SSH | Запрет `root` и паролей, смена порта, проверка наличия `authorized_keys` | Вкл |
| `fail2ban_ufw` | Fail2Ban + UFW | Защита SSH (`systemd`), закрытие всех портов кроме SSH (и `3389/tcp` на `tailscale0` при выборе Tailscale) | Вкл |
| `bbr` | TCP BBR | Включение `bbr` + `fq` в `sysctl` для плавной работы RDP | Вкл |
| `swap` | Swap-файл 2 ГБ | Файл подкачки `/swapfile` с записью в `/etc/fstab` | Вкл |
| `unattended` | Автообновления | Фоновая установка патчей безопасности (`unattended-upgrades`) | Вкл |
| `tailscale` | Tailscale | Приватная mesh-сеть для прямого доступа к RDP без SSH-туннеля | Выкл |

### Графика (UI), терминал и оболочка
| ID | Модуль | Описание | По умолч. |
|---|---|---|---|
| `xfce` | XFCE | Рабочий стол + удаление `xfce4-screensaver` и `light-locker` (фикс черного экрана в RDP) | Вкл |
| `xrdp` | XRDP | Сервер RDP на `127.0.0.1:3389` (или через `tailscale0`), фикс `startwm.sh` и `polkit` (`colord`) | Вкл |
| `kitty_fonts` | Kitty + Nerd Fonts | Терминал Kitty по умолчанию в XFCE + шрифты `JetBrainsMono` и `FiraCode` | Вкл |
| `ru_layout` | Русская раскладка | Локаль `ru_RU.UTF-8`, переключение `us,ru` по `Alt+Shift` в системе и XFCE | Вкл |
| `zsh_starship` | Zsh + Starship | Zsh по умолчанию, промпт `nerd-font-symbols`, `zsh-autosuggestions`, `zsh-syntax-highlighting` | Вкл |

### Разработка, контейнеры и ИИ
| ID | Модуль | Описание | По умолч. |
|---|---|---|---|
| `ai_cli` | AI CLI-инструменты | Claude Code, Codex, OpenCode, Gemini CLI, Aider + Node.js 22 LTS, `uv`/`uvx`, `gh`, `ripgrep`, `fd-find`, `jq` | Вкл |
| `docker` | Docker Engine | Docker CE, Compose v2, Buildx, группа `docker`, ротация логов (`daemon.json`) | Выкл |
| `ollama` | Ollama | Локальный сервер LLM-моделей на `127.0.0.1:11434` | Выкл |

---

## Быстрый старт

```bash
go mod tidy
make build-linux      # x86_64 -> dist/vps-setup-linux-amd64
make build-arm64      # ARM64  -> dist/vps-setup-linux-arm64
```

### Интерактивный запуск на сервере
```bash
chmod +x vps-setup-linux-amd64
sudo ./vps-setup-linux-amd64
```

### Подключение к RDP через SSH-туннель
```bash
ssh -p 2222 -N -L 127.0.0.1:13389:localhost:3389 user@<IP_СЕРВЕРА>
```

---

## Флаги CLI

| Флаг | Описание |
|---|---|
| `--status` | Вывести таблицу состояния всех модулей (работает без `sudo`) |
| `--json` | Вывод статуса в JSON (используется совместно с `--status`) |
| `--only <list>` | Неинтерактивный запуск указанных модулей через запятую (`--only xfce,xrdp,docker`) |
| `--user <name>` | Явно указать целевого пользователя (для `cloud-init` / Ansible под `root`) |
| `--ssh-port <port>` | Порт SSH для неинтерактивного режима (по умолчанию `2222`) |
| `--auto-reboot` | Отложенная перезагрузка (5 сек) после `upgrade_ubuntu`, если требуется |
| `--dry-run` | Показать план команд и файлов без внесения изменений в систему |
| `--self-update` | Обновить бинарник из GitHub Releases |
| `--version` | Показать версию и платформу |
