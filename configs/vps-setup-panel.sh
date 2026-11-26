#!/bin/bash
# Crystal-Core VPS Install Manager: значки установленных программ на нижней панели XFCE.
#
#   panel.sh sync    — привести панель в соответствие со списком программ в $REG:
#                      добавить новые значки (каждый один раз — если пользователь уберёт
#                      значок сам, он не вернётся) и убрать значки удалённых программ;
#   panel.sh remove  — убрать все значки, добавленные программой;
#   panel.sh add     — то же, что sync (совместимость с версией 1.1.0).
#
# Работает внутри XFCE-сессии пользователя (через xfconf). Запускается автозапуском при
# входе, а установщиком — в уже открытых сессиях.
set -u

REG=${VPS_SETUP_PANEL_DIR:-/usr/share/vps-setup/panel}
PREFIX=crystal-core-
CH=xfce4-panel
CONF=${XDG_CONFIG_HOME:-$HOME/.config}
MARKS=$CONF/vps-setup/panel
OLD_MARKER=$CONF/vps-setup/panel-added
MODE=${1:-sync}

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

# Печатает «id имя.desktop» для каждого значка-запускателя, добавленного программой.
our_plugins() {
	local prop id item
	for prop in $(xfconf-query -c "$CH" -l 2>/dev/null | grep -E '^/plugins/plugin-[0-9]+/items$'); do
		id=${prop#/plugins/plugin-}
		id=${id%/items}
		while read -r item; do
			case "$item" in
				"$PREFIX"*.desktop) echo "$id $item" ;;
			esac
		done < <(values "$prop")
	done
}

find_plugin() {
	our_plugins | awk -v n="$1" '$2 == n {print $1; exit}'
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

add_plugin() {
	local name=$1 src=$2 panel max id i pos=-1 type
	local -a cur=() new=()
	panel=$(bottom_panel)
	[ -n "$panel" ] || return 0
	max=$(xfconf-query -c "$CH" -l | sed -nE 's|^/plugins/plugin-([0-9]+)$|\1|p' | sort -n | tail -n 1)
	id=$(( ${max:-0} + 1 ))

	mkdir -p "$CONF/xfce4/panel/launcher-$id"
	cp "$src" "$CONF/xfce4/panel/launcher-$id/$name"
	xfconf-query -c "$CH" -p "/plugins/plugin-$id" -n -t string -s launcher
	xfconf-query -c "$CH" -p "/plugins/plugin-$id/items" -n -a -t string -s "$name"

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
}

remove_plugin() {
	local id=$1 panel i
	local -a cur=() keep=()
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
}

restart_panel() {
	(xfce4-panel -r >/dev/null 2>&1 &)
}

do_sync() {
	local changed=0 id name src
	wait_panel || exit 0
	mkdir -p "$MARKS"
	# Отметка версии 1.1.0 относилась к значку самой программы.
	if [ -f "$OLD_MARKER" ]; then
		touch "$MARKS/${PREFIX}vps-setup.desktop"
		rm -f "$OLD_MARKER"
	fi

	# Программа удалена — убираем её значок. Список берём целиком заранее:
	# remove_plugin меняет конфигурацию, по которой он строится.
	local -a plugs=()
	local line
	mapfile -t plugs < <(our_plugins)
	for line in "${plugs[@]}"; do
		read -r id name <<<"$line"
		if [ ! -f "$REG/$name" ]; then
			remove_plugin "$id"
			rm -f "$MARKS/$name"
			changed=1
		fi
	done

	# Новые программы — добавляем значок (один раз).
	for src in "$REG"/"$PREFIX"*.desktop; do
		[ -f "$src" ] || continue
		name=${src##*/}
		id=$(find_plugin "$name")
		if [ -n "$id" ]; then
			# Значок уже есть — обновляем файл ярлыка (например, после переустановки).
			mkdir -p "$CONF/xfce4/panel/launcher-$id"
			cp "$src" "$CONF/xfce4/panel/launcher-$id/$name"
			touch "$MARKS/$name"
		elif [ ! -f "$MARKS/$name" ]; then
			add_plugin "$name" "$src"
			touch "$MARKS/$name"
			changed=1
		fi
	done

	if [ "$changed" = 1 ]; then
		restart_panel
	fi
}

do_remove() {
	local changed=0 id name line
	local -a plugs=()
	mapfile -t plugs < <(our_plugins)
	for line in "${plugs[@]}"; do
		read -r id name <<<"$line"
		remove_plugin "$id"
		changed=1
	done
	rm -rf "$MARKS" "$OLD_MARKER"
	rmdir "$CONF/vps-setup" 2>/dev/null || true
	if [ "$changed" = 1 ]; then
		restart_panel
	fi
}

case "$MODE" in
	sync | add) do_sync ;;
	remove) do_remove ;;
	*) echo "usage: $0 sync|remove" >&2; exit 2 ;;
esac
