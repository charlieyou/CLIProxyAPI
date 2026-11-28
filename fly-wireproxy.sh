#!/bin/sh
# Runs wireproxy for the Fly deployment and restarts it when it exits or when the
# WireGuard Endpoint host resolves to a new IP, because wireproxy resolves the
# endpoint only at startup (the home endpoint is a dynamic DNS name).
conf=${1:-/data/wireproxy.conf}

resolve() {
	host=$(sed -n 's/^Endpoint *= *\(.*\):[0-9]*$/\1/p' "$conf")
	getent ahostsv4 "$host" | awk 'NR == 1 { print $1 }'
}

while true; do
	ip=$(resolve)
	wireproxy -s -c "$conf" &
	pid=$!
	while kill -0 "$pid" 2>/dev/null; do
		sleep 60
		new=$(resolve)
		if [ -n "$new" ] && [ "$new" != "$ip" ]; then
			echo "wireproxy endpoint moved from $ip to $new; restarting"
			kill "$pid"
			break
		fi
	done
	wait "$pid"
	sleep 5
done
