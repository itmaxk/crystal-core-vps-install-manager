package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

const (
	actionStatus = "status"
	actionUpdate = "update"
	actionRemove = "remove"
	actionExit   = "exit"
	actionStart  = "start"
	actionBack   = "back"
	removeSelect = "select"
	removeAll    = "all"
)

const backHint = "Esc — назад в главное меню"

// formKeyMap: Esc (как и Ctrl+C) закрывает текущую форму. В подменю это означает
// «назад в главное меню», в главном меню — выход из программы.
func formKeyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "назад"))
	return km
}

func newForm(groups ...*huh.Group) *huh.Form {
	return huh.NewForm(groups...).WithKeyMap(formKeyMap())
}

// interactiveLoop — главное меню. Из любого подменю можно вернуться назад (Esc или пункт
// «← Назад»); программа завершается только после выполнения операции или по «Выход».
func interactiveLoop(base Config, autoReboot bool) {
	startUpdateCheck()
	for {
		action, err := mainMenu(base)
		if err != nil || action == actionExit {
			fmt.Println(dimStyle.Render("До встречи!"))
			return
		}

		cfg := base
		switch action {
		case actionStatus:
			printStatusTable(cfg)

		case actionUpdate:
			updateMenu()

		case presetRDP, presetFull:
			ok, err := installForm(&cfg, action)
			if err != nil && !isAbort(err) {
				fmt.Println(errStyle.Render("✖ " + err.Error()))
			}
			if !ok {
				continue
			}
			cfg.Modules = orderModules(cfg.Modules)
			if err := validatePlan(&cfg); err != nil {
				fmt.Println(errStyle.Render("✖ " + err.Error()))
				continue
			}
			runInstall(cfg, autoReboot)
			return

		case actionRemove:
			ok, err := removeForm(&cfg)
			if err != nil && !isAbort(err) {
				fmt.Println(errStyle.Render("✖ " + err.Error()))
			}
			if !ok {
				continue
			}
			if err := validateRemovePlan(&cfg); err != nil {
				fmt.Println(errStyle.Render("✖ " + err.Error()))
				continue
			}
			runRemove(cfg)
			return
		}
	}
}

func mainMenu(cfg Config) (string, error) {
	action := presetFull
	if !hasBin("xfce4-session") || !serviceActive("xrdp") {
		action = presetRDP
	}
	err := newForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Главное меню").
			Description("↑/↓ — выбор, Enter — открыть, Esc — выход").
			Options(
				huh.NewOption("Быстрый старт — рабочий стол XFCE + вход по RDP (для первого запуска)", presetRDP),
				huh.NewOption("Полная настройка — все рекомендуемые модули", presetFull),
				huh.NewOption("Удаление установленных компонентов", actionRemove),
				huh.NewOption("Состояние модулей", actionStatus),
				huh.NewOption(updateMenuLabel(), actionUpdate),
				huh.NewOption("Выход", actionExit),
			).
			Value(&action),
	)).Run()
	if err != nil {
		return actionExit, err
	}
	return action, nil
}

func buildMenu(cfg Config, ids []string) []huh.Option[string] {
	nameStyle := lipgloss.NewStyle().Bold(true)
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	badgeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575"))

	var options []huh.Option[string]
	for _, m := range getModuleDefs() {
		if ids != nil && !contains(ids, m.ID) {
			continue
		}
		label := nameStyle.Render(m.Title) + descStyle.Render(" — "+m.Description)
		if m.IsReady(cfg) {
			label += badgeStyle.Render(" [установлено]")
		}
		options = append(options, huh.NewOption(label, m.ID))
	}
	return options
}

// installForm возвращает true, если пользователь подтвердил установку.
func installForm(cfg *Config, preset string) (bool, error) {
	cfg.Modules, _ = presetModules(preset, *cfg)
	title := "Полная настройка"
	if preset == presetRDP {
		title = "Быстрый старт: рабочий стол + RDP"
	}
	needRDPPassword := cfg.TargetUser != "root" && !userHasPassword(cfg.TargetUser)
	var confirmNew, confirmRDP string
	decision := actionStart

	err := newForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title(title+" — выберите модули").
				Description("Пробел — отметить/снять, Enter — далее. Модули выполняются сверху вниз. "+backHint).
				Height(20).
				Options(buildMenu(*cfg, nil)...).
				Value(&cfg.Modules).
				Validate(func(v []string) error {
					if len(v) == 0 {
						return fmt.Errorf("отметьте хотя бы один модуль (или Esc — назад)")
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
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Всё готово").
				DescriptionFunc(func() string {
					return "Будет установлено: " + fmt.Sprint(orderModules(cfg.Modules))
				}, &cfg.Modules).
				Options(
					huh.NewOption("▶ Начать установку", actionStart),
					huh.NewOption("← Назад в главное меню", actionBack),
				).
				Value(&decision),
		),
	).Run()
	if err != nil {
		return false, err
	}
	return decision == actionStart, nil
}

// removeForm возвращает true, если пользователь подтвердил удаление; cfg.Modules — что удалять.
func removeForm(cfg *Config) (bool, error) {
	candidates := removableModules(*cfg)
	if len(candidates) == 0 {
		fmt.Println(warnStyle.Render("Удалять нечего: программа ничего не устанавливала или всё уже удалено."))
		return false, nil
	}

	mode := removeSelect
	err := newForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Удаление установленных компонентов").
			Description("Удаляется только то, что поставила эта программа. "+backHint).
			Options(
				huh.NewOption("Выбрать модули для удаления", removeSelect),
				huh.NewOption(fmt.Sprintf("Удалить всё, что установлено программой (%d)", len(candidates)), removeAll),
				huh.NewOption("← Назад в главное меню", actionBack),
			).
			Value(&mode),
	)).Run()
	if err != nil || mode == actionBack {
		return false, err
	}

	cfg.Modules = nil
	if mode == removeAll {
		cfg.Modules = append([]string(nil), candidates...)
	}
	decision := actionBack

	err = newForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Что удалить?").
				Description("Пробел — отметить/снять, Enter — далее. "+backHint).
				Height(20).
				Options(buildMenu(*cfg, candidates)...).
				Value(&cfg.Modules).
				Validate(func(v []string) error {
					if len(v) == 0 {
						return fmt.Errorf("отметьте хотя бы один модуль (или Esc — назад)")
					}
					return nil
				}),
		),
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Подтвердите удаление").
				DescriptionFunc(func() string { return removalSummary(cfg.Modules) }, &cfg.Modules).
				Options(
					huh.NewOption("← Назад в главное меню", actionBack),
					huh.NewOption("✖ Да, удалить", actionRemove),
				).
				Value(&decision),
		),
	).Run()
	if err != nil {
		return false, err
	}
	cfg.Modules = orderForRemoval(cfg.Modules)
	return decision == actionRemove, nil
}

// --- Обновление программы из меню ---

// Проверка новой версии идёт в фоне с момента запуска, чтобы не задерживать меню:
// если релиз новее, пункт меню подписывается «доступна vX».
var updateState struct {
	sync.Mutex
	done bool
	rel  githubRelease
	err  error
}

func startUpdateCheck() {
	go func() {
		rel, err := fetchLatestRelease(10 * time.Second)
		updateState.Lock()
		updateState.done, updateState.rel, updateState.err = true, rel, err
		updateState.Unlock()
	}()
}

func cachedLatestRelease() (githubRelease, error, bool) {
	updateState.Lock()
	defer updateState.Unlock()
	return updateState.rel, updateState.err, updateState.done
}

func updateMenuLabel() string {
	label := "Обновить программу (сейчас " + Version + ")"
	if rel, err, done := cachedLatestRelease(); done && err == nil && isNewerVersion(rel.TagName, Version) {
		label = "Обновить программу — доступна " + rel.TagName + " (сейчас " + Version + ")"
	}
	return label
}

func updateMenu() {
	rel, err, done := cachedLatestRelease()
	if !done || err != nil {
		err = withSpinner("Проверка обновлений", func() error {
			var e error
			rel, e = fetchLatestRelease(20 * time.Second)
			return e
		})
		updateState.Lock()
		updateState.done, updateState.rel, updateState.err = err == nil, rel, err
		updateState.Unlock()
	}
	if err != nil {
		fmt.Println(errStyle.Render("✖ " + err.Error()))
		return
	}
	if !isNewerVersion(rel.TagName, Version) {
		fmt.Println(okStyle.Render("✓ Установлена последняя версия (" + Version + ")"))
		return
	}

	decision := actionUpdate
	desc := "После обновления программа перезапустится уже в новой версии. " + backHint
	if rel.HTMLURL != "" {
		desc = "Что нового: " + rel.HTMLURL + "\n" + desc
	}
	err = newForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(fmt.Sprintf("Доступна версия %s (у вас %s)", rel.TagName, Version)).
			Description(desc).
			Options(
				huh.NewOption("⬇ Обновить и перезапустить", actionUpdate),
				huh.NewOption("← Назад в главное меню", actionBack),
			).
			Value(&decision),
	)).Run()
	if err != nil || decision != actionUpdate {
		return
	}

	path, err := applyUpdate(rel)
	if err != nil {
		fmt.Println(errStyle.Render("✖ Ошибка обновления: " + err.Error()))
		return
	}
	if dryRun {
		return
	}
	fmt.Println(dimStyle.Render("Перезапуск в версии " + rel.TagName + "..."))
	if err := syscall.Exec(path, os.Args, os.Environ()); err != nil {
		fmt.Println(warnStyle.Render("Не удалось перезапуститься автоматически (" + err.Error() + "). Запустите заново: sudo vps-setup"))
		os.Exit(0)
	}
}
