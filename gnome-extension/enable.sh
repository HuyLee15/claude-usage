#!/bin/sh
# Adds the extension to org.gnome.shell enabled-extensions. `gnome-extensions enable` cannot be used here
# because the running shell only learns about a newly copied extension after it reloads.
set -eu
uuid="$1"
current=$(gsettings get org.gnome.shell enabled-extensions)
case "$current" in
	*"'$uuid'"*) echo "already enabled: $uuid"; exit 0 ;;
	"@as []" | "[]") updated="['$uuid']" ;;
	*) updated="${current%]}, '$uuid']" ;;
esac
gsettings set org.gnome.shell enabled-extensions "$updated"
echo "enabled: $uuid"
