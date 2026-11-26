#!/bin/bash
# Crystal-Core VPS Install Manager: значок программы на нижней панели XFCE.
#   panel.sh add     — добавить (один раз: если пользователь уберёт значок сам, он не вернётся)
#   panel.sh remove  — убрать
# Работает внутри XFCE-сессии пользователя (через xfconf). Запускается автозапуском
# при входе, а установщиком — в уже открытых сессиях.
set -u

NAME=crystal-core-vps-setup.desktop
SRC=${VPS_SETUP_DESKTOP:-/usr/share/applications/$NAME}
CH=xfce4-panel
CONF=${XDG_CONFIG_HOME:-$HOME/.config}
MARKER=$CONF/vps-setup/panel-added
MODE=${1:-add}

command -v xfconf-query >/dev/null 2>&1 || exit 0

# Значения массива: xfconf-query печатает «Value is an array with N items:», пустую строку и значения.
values() {
	xfconf-query -c "$CH" -p "$1" 2>/dev/null | tail -n +3
}

wait_panel() {
	local i
	for i in $(seq 1 "${VPS_SETUP_PANEL_WAIT:-120}"); do
		if [ -n "$(values /panels)" ]; then
			return 0
		fi
		sleep 1
	done
	return 1
}

find_plugin() {
	local prop id
	for prop in $(xfconf-query -c "$CH" -l 2>/dev/null | grep -E '^/plugins/plugin-[0-9]+/items$'); do
		if values "$prop" | grep -qxF "$NAME"; then
			id=${prop#/plugins/plugin-}
			echo "${id%/items}"
			return 0
		fi
	done
}

# Нижняя панель: position p=10 (в стандартной раскладке XFCE это panel-2), иначе последняя.
bottom_panel() {
	local id last=""
	for id in $(values /panels); do
		last=$id
		case "$(xfconf-query -c "$CH" -p "/panels/panel-$id/position" 2>/dev/null)" in
			p=10\;*) echo "$id"; return 0 ;;
		esac
	done
	echo "$last"
}

set_ids() {
	local panel=$1 i
	shift
	local -a args=()
	for i in "$@"; do
		args+=(-t int -s "$i")
	done
	xfconf-query -c "$CH" -p "/panels/panel-$panel/plugin-ids" -n -a "${args[@]}"
}

restart_panel() {
	(xfce4-panel -r >/dev/null 2>&1 &)
}

do_add() {
	[ -f "$SRC" ] || exit 0
	[ -f "$MARKER" ] && exit 0
	wait_panel || exit 0

	local id panel max i pos=-1 type
	local -a cur=() new=()
	id=$(find_plugin)
	if [ -n "$id" ]; then
		# Значок уже в конфигурации панели (например, после переустановки) — восстанавливаем файл.
		mkdir -p "$CONF/xfce4/panel/launcher-$id"
		cp "$SRC" "$CONF/xfce4/panel/launcher-$id/$NAME"
	else
		panel=$(bottom_panel)
		[ -n "$panel" ] || exit 0
		max=$(xfconf-query -c "$CH" -l | sed -nE 's|^/plugins/plugin-([0-9]+)$|\1|p' | sort -n | tail -n 1)
		id=$(( ${max:-0} + 1 ))

		mkdir -p "$CONF/xfce4/panel/launcher-$id"
		cp "$SRC" "$CONF/xfce4/panel/launcher-$id/$NAME"
		xfconf-query -c "$CH" -p "/plugins/plugin-$id" -n -t string -s launcher
		xfconf-query -c "$CH" -p "/plugins/plugin-$id/items" -n -a -t string -s "$NAME"

		# Ставим сразу после последнего значка-запускателя, рядом с терминалом и браузером.
		mapfile -t cur < <(values "/panels/panel-$panel/plugin-ids")
		for i in "${!cur[@]}"; do
			type=$(xfconf-query -c "$CH" -p "/plugins/plugin-${cur[$i]}" 2>/dev/null || true)
			if [ "$type" = launcher ]; then
				pos=$i
			fi
		done
		if [ "$pos" -lt 0 ]; then
			pos=$(( ${#cur[@]} - 1 ))
		fi
		for i in "${!cur[@]}"; do
			new+=("${cur[$i]}")
			if [ "$i" -eq "$pos" ]; then
				new+=("$id")
			fi
		done
		if [ "${#cur[@]}" -eq 0 ]; then
			new=("$id")
		fi
		set_ids "$panel" "${new[@]}"
		restart_panel
	fi
	mkdir -p "$(dirname "$MARKER")"
	touch "$MARKER"
}

do_remove() {
	local id panel i
	local -a cur=() keep=()
	rm -f "$MARKER"
	rmdir "$(dirname "$MARKER")" 2>/dev/null || true
	id=$(find_plugin)
	[ -n "$id" ] || exit 0
	for panel in $(values /panels); do
		mapfile -t cur < <(values "/panels/panel-$panel/plugin-ids")
		keep=()
		for i in "${cur[@]}"; do
			if [ "$i" != "$id" ]; then
				keep+=("$i")
			fi
		done
		if [ "${#keep[@]}" -ne "${#cur[@]}" ] && [ "${#keep[@]}" -gt 0 ]; then
			set_ids "$panel" "${keep[@]}"
		fi
	done
	xfconf-query -c "$CH" -p "/plugins/plugin-$id" -r -R 2>/dev/null || true
	rm -rf "$CONF/xfce4/panel/launcher-$id"
	restart_panel
}

case "$MODE" in
	add) do_add ;;
	remove) do_remove ;;
	*) echo "usage: $0 add|remove" >&2; exit 2 ;;
esac
