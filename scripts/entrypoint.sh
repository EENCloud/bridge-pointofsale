#!/bin/sh
set -e

echo "Starting Bridge POS Service..."
echo "Environment MODE: ${MODE:-not_set}"

# --- Create container directories ---
mkdir -p /opt/een/var/log/point_of_sale
mkdir -p /opt/een/data/point_of_sale/audit_logs
mkdir -p /opt/een/data/point_of_sale/badger_db

# --- Configure iptables for Seven Eleven port ---
IPTABLES_CONF="/opt/een/data/etc/iptables.d/filter/71-seven_eleven.conf"
mkdir -p "$(dirname "$IPTABLES_CONF")"
if [ ! -f "$IPTABLES_CONF" ]; then
    echo "-A INPUT -m state --state NEW -m tcp -p tcp --dport 6334 -j ACCEPT" > "$IPTABLES_CONF"
fi

# --- Start process manager ---
exec /usr/bin/supervisord -n 