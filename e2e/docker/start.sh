#!/bin/sh

# Start log generator in background
/usr/local/bin/loggen -file /var/log/testapp.log -server "${HOSTNAME:-server}" -interval 200ms &

# Start SSH daemon
exec /usr/sbin/sshd -D -e
