#!/bin/sh
# udhcpc's lease script, run by /init. $1 is the event. The lease is in the environment: $interface, $ip, $mask (a
# prefix length), and $router and $dns (space-separated lists, either may be unset).
case $1 in
deconfig)
	ip addr flush dev "${interface:?}"
	ip link set dev "$interface" up
	;;
bound)
	ip addr add "${ip:?}/${mask:?}" dev "${interface:?}"
	for gw in ${router-}; do
		ip route add default via "$gw" dev "$interface" && break
	done
	: >/etc/resolv.conf
	for ns in ${dns-}; do
		echo "nameserver $ns" >>/etc/resolv.conf
	done
	;;
esac
