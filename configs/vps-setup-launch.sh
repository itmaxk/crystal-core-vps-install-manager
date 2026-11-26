#!/bin/bash
# Crystal-Core VPS Install Manager: запуск из меню или с панели XFCE.
# Окно терминала не закрывается сразу, чтобы был виден результат.
if [ "$(id -u)" -eq 0 ]; then
	/usr/local/bin/vps-setup "$@"
else
	sudo /usr/local/bin/vps-setup "$@"
fi
echo
read -r -p "Нажмите Enter, чтобы закрыть окно..." _
