#!/bin/sh
# Fix the data volume ownership, then run skilld-server as the unprivileged skilld user.
# Docker sends SIGTERM to PID 1; exec replaces this shell so the server receives it.
set -eu

data_dir="${DATA_DIR:-/var/lib/skilld}"
mkdir -p "$data_dir"
# SQLite creates the database with the process umask. Keep the vault unreadable
# by other users when the data directory is a host mount.
umask 077

if [ "$(id -u)" -eq 0 ]; then
  chown -R skilld:skilld "$data_dir"
  exec /usr/sbin/gosu skilld /usr/local/bin/skilld-server "$@"
fi

exec /usr/local/bin/skilld-server "$@"
