#!/bin/sh
# QEMU end-to-end test (design §7 criterion 6). Run it as `task e2e`; it needs DOCKER_HOST and nothing else
# on the host (see test/e2e/README.md). Boots amd64 UEFI, amd64 BIOS and arm64 UEFI VMs through hephaestus
# in file mode, then amd64 UEFI again with hephaestus behind Caddy. Prints a PASS or FAIL line per check,
# writes every log to build/e2e/<UTC timestamp>/, and removes everything it created on any exit.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$root"
: "${DOCKER_HOST:?set DOCKER_HOST, for colima: unix://\$HOME/.colima/default/docker.sock}"
export DOCKER_HOST

# --- pins and topology (the spike's, design §7) ---
# Caddy is pinned by the digest of its multi-arch index (caddy:2 on 2026-11-15); the toolbox pins its own base.
CADDY_IMAGE=docker.io/library/caddy:2@sha256:4e2425f6009577e7c651010f16679c28f7d12df3e7b1914f941a7a3370746b3c
TOOLS=hephaestus-e2e-qemu:local
IMAGE=hephaestus:e2e
PLATFORM=linux/arm64 # same as `task ipxe:build:testca` and image:build; colima's native arch
NET=heph-e2e
VOL=heph-e2e-data
GATEWAY=10.99.0.1
DNS_IP=10.99.0.2
HEPH_IP=10.99.0.10
CADDY_IP=10.99.0.11
QEMU_IP=10.99.0.20
FQDN=hephaestus.e2e.test
BOOT_TIMEOUT=120 # seconds per boot
TASK=${TASK_EXE:-task}

ts=$(date -u +%Y%m%dT%H%M%SZ)
out=build/e2e/$ts
certs=build/e2e/run/certs
testca_pem=build/ipxe/out/e2e-ca.pem
testca_dir=build/ipxe/out/testca
src=
heph_mode='file'
passes=0
fails=0
run_start=$(date +%s)

pass() {
	printf 'PASS %s\n' "$*"
	passes=$((passes + 1))
}
fail() {
	printf 'FAIL %s\n' "$*"
	fails=$((fails + 1))
}
# check <description> <command...>: PASS when the command succeeds.
check() {
	desc=$1
	shift
	if "$@"; then pass "$desc"; else fail "$desc"; fi
}
step() { printf '== %s\n' "$*"; }
ge() { [ -n "$1" ] && [ "$1" -ge "$2" ] 2>/dev/null; } # ge <n> <min>: a missing or non-numeric n fails

box() { docker exec -i heph-e2e-qemu "$@"; }
# jn <file> <jq args...> <filter>: how many JSON log lines in <file> the filter selects. Non-JSON lines are skipped.
jn() {
	f=$1
	shift
	box jq -Rc "$@" <"$f" | grep -c . || true
}
sqlite_q() { docker run --rm --user 65532:65532 -v "$VOL":/d "$TOOLS" sqlite3 -batch -cmd '.timeout 5000' /d/hephaestus.db "$1"; }
sidecar() { docker run --rm --network container:heph-e2e-hephaestus "$TOOLS" "$@"; }
hold_count() {
	sidecar curl -fsS http://127.0.0.1:9090/metrics |
		awk '/^hephaestus_boot_decisions_total\{/ && /action="hold"/ && /decision_row="7"/ {s += $NF} END {print s + 0}'
}
mark() { docker logs "$1" 2>&1 | wc -l | tr -d ' '; }
since() { docker logs "$1" 2>&1 | tail -n +$(($2 + 1)); }

save_logs() {
	for c in dnsmasq hephaestus caddy qemu; do
		if docker inspect "heph-e2e-$c" >/dev/null 2>&1; then
			case $c in hephaestus | dnsmasq) label=$c-$heph_mode ;; *) label=$c ;; esac
			docker logs "heph-e2e-$c" >"$out/$label.log" 2>&1 || true
		fi
	done
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	set +e
	save_logs
	for c in $(docker ps -aq --filter 'name=^heph-e2e-'); do docker rm -f "$c" >/dev/null 2>&1; done
	docker network rm "$NET" >/dev/null 2>&1
	docker volume rm "$VOL" >/dev/null 2>&1
	[ -n "$src" ] && rm -rf "${src:?}"
	rm -rf "${certs:?}"
	rm -f "${testca_pem:?}"
	rm -rf "${testca_dir:?}"
	if [ "$status" -eq 0 ] && [ "$fails" -gt 0 ]; then status=1; fi
	printf 'e2e: %s passed, %s failed in %ss; logs in %s\n' "$passes" "$fails" "$(($(date +%s) - run_start))" "$out"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- containers ---
remove_all() {
	for c in $(docker ps -aq --filter 'name=^heph-e2e-'); do docker rm -f "$c" >/dev/null; done
	docker network rm "$NET" >/dev/null 2>&1 || true
	docker volume rm "$VOL" >/dev/null 2>&1 || true
}

start_dnsmasq() { # <hephaestus.e2e.test address>
	docker run -d --name heph-e2e-dnsmasq --network "$NET" --ip "$DNS_IP" --cap-add NET_ADMIN \
		-v "$root/test/e2e:/e2e:ro" "$TOOLS" \
		dnsmasq -C /e2e/dnsmasq.conf --address=/$FQDN/"$1" >/dev/null
	wait_for 'dnsmasq ready' 20 sh -c 'docker logs heph-e2e-dnsmasq 2>&1 | grep -q "DHCP, IP range"'
}

start_hephaestus() { # file | proxy
	heph_mode=$1
	set -- -d --name heph-e2e-hephaestus --network "$NET" --ip "$HEPH_IP" --user 65532:65532 \
		--cap-drop ALL --cap-add NET_BIND_SERVICE --sysctl net.ipv4.ip_unprivileged_port_start=1024 \
		-v "$VOL":/var/lib/hephaestus \
		-e HEPHAESTUS_FQDN="$FQDN" -e HEPHAESTUS_PROVISIONING_INTERFACE=eth0 \
		-e HEPHAESTUS_PROVISIONING_ADDRESS="$HEPH_IP" -e HEPHAESTUS_PROVISIONING_SUBNET=10.99.0.0/24 \
		-e HEPHAESTUS_TLS_MODE="$heph_mode" -e HEPHAESTUS_LOG_LEVEL=debug
	case $heph_mode in
	file) set -- "$@" -v "$root/$certs:/certs:ro" -e HEPHAESTUS_TLS_CERT_FILE=/certs/fullchain.pem -e HEPHAESTUS_TLS_KEY_FILE=/certs/privkey.pem ;;
	proxy) set -- "$@" -e HEPHAESTUS_PROXY_ADDRESS="$HEPH_IP:8443" -e HEPHAESTUS_PROXY_TRUSTED="$CADDY_IP/32" ;;
	esac
	docker run "$@" "$IMAGE" >/dev/null
	wait_for "hephaestus ready ($heph_mode)" 60 sidecar curl -fsS -o /dev/null http://127.0.0.1:9090/readyz
}

wait_for() { # <what> <seconds> <command...>
	what=$1 limit=$2
	shift 2
	n=0
	until "$@" >/dev/null 2>&1; do
		n=$((n + 1))
		if [ "$n" -gt "$limit" ]; then
			echo "e2e: timed out waiting for $what" >&2
			return 1
		fi
		sleep 1
	done
}

# unknown_action defaults to local (row 7 exit); M1 has no UI, so seed hold straight into the database.
seed_hold() {
	sqlite_q "UPDATE settings SET value='hold' WHERE key='unknown_action'"
	[ "$(sqlite_q "SELECT value FROM settings WHERE key='unknown_action'")" = hold ]
}

# --- run-level checks ---
# tls_checks <label> <ip>: TLS 1.2 is refused, and TLS 1.3 completes against the test CA with no session ticket
# (iPXE 9261ebe hashes a NewSessionTicket sent before its Finished into the transcript and fails the handshake).
tls_checks() {
	label=$1 ip=$2
	probe="printf 'GET /e2e-probe HTTP/1.1\r\nHost: $FQDN\r\nConnection: close\r\n\r\n' | openssl s_client -ign_eof -connect $ip:443 -servername $FQDN -CAfile /certs/ca.pem"
	tls12=$(docker run --rm --network "$NET" -v "$root/$certs:/certs:ro" "$TOOLS" sh -c "$probe -tls1_2 2>&1" || true)
	printf '%s\n' "$tls12" >"$out/tls12-$label.txt"
	check "$label: openssl s_client -tls1_2 gets a protocol-version alert" sh -c 'grep -qi "alert protocol version" "$1"' _ "$out/tls12-$label.txt"
	tls13=$(docker run --rm --network "$NET" -v "$root/$certs:/certs:ro" "$TOOLS" sh -c "$probe -tls1_3 2>&1" || true)
	printf '%s\n' "$tls13" >"$out/tls13-$label.txt"
	check "$label: TLS 1.3 handshake verifies against the test CA" sh -c 'grep -q "Verify return code: 0 (ok)" "$1" && grep -q "TLSv1.3" "$1"' _ "$out/tls13-$label.txt"
	# -ign_eof keeps s_client reading until the server closes: without it s_client quits at stdin EOF before it
	# reads a post-handshake ticket, and this check would pass whether or not one was sent. The 404 proves it read.
	check "$label: the probe read the server's response after the handshake (HTTP 404)" grep -q '^HTTP/1.1 404' "$out/tls13-$label.txt"
	check "$label: no NewSessionTicket sent after the handshake" sh -c '! grep -qi "New Session Ticket" "$1"' _ "$out/tls13-$label.txt"
}

# listens <tcp|udp> <port> <ss -lnut output>: the output has a listener on the port (column 5 is the local address).
listens() { awk -v proto="$1" -v port="$2" '$1 == proto && $5 ~ (":" port "$") {f = 1} END {exit !f}' "$3"; }
# runs_as_65532 <proc status file>: the real and effective UIDs are both 65532.
runs_as_65532() { awk '/^Uid:/ {print $2, $3}' "$1" | grep -qx '65532 65532'; }

capability_checks() {
	set +e
	nocaps=$(docker run --rm --user 65532:65532 --cap-drop ALL "$IMAGE" 2>&1)
	rc=$?
	set -e
	printf '%s\n' "$nocaps" >"$out/cap-drop-all.txt"
	if [ "$rc" -ne 0 ]; then rc_ok=yes; else rc_ok=no; fi
	check "--cap-drop ALL without the cap-add fails to exec (file capability)" sh -c '[ "$1" = yes ] && grep -q "operation not permitted" "$2"' _ "$rc_ok" "$out/cap-drop-all.txt"

	docker run --rm --network container:heph-e2e-hephaestus "$TOOLS" ss -H -lnut >"$out/listeners.txt" 2>&1 || true
	for p in "udp 67" "udp 69" "udp 4011" "tcp 80" "tcp 443"; do
		# shellcheck disable=SC2086 # deliberate word split into protocol and port
		set -- $p
		check "CAP_NET_BIND_SERVICE: hephaestus listens on $1 $2" listens "$1" "$2" "$out/listeners.txt"
	done
	docker run --rm --pid container:heph-e2e-hephaestus --cap-add SYS_PTRACE "$TOOLS" grep -E '^(Uid|Gid|CapEff|NoNewPrivs):' /proc/1/status >"$out/proc-status.txt" 2>&1 || true
	check "hephaestus runs as UID 65532, not root" runs_as_65532 "$out/proc-status.txt"
}

# --- one boot ---
# boot <name> <firmware> <mac> <arch> <offered boot file> <sent file> <file|proxy>
boot() {
	name=$1 fw=$2 mac=$3 arch=$4 offered=$5 sent=$6 mode=$7
	uuid=e2e00000-0000-4000-8000-$(printf '%s' "$mac" | tr -d :)
	serial=$out/$name.serial.log
	uefi=yes
	[ "$fw" = seabios ] && uefi=no
	step "boot $name ($fw, $arch, $mode)"
	hm=$(mark heph-e2e-hephaestus)
	[ "$mode" = proxy ] && cm=$(mark heph-e2e-caddy)
	hold0=$(hold_count)
	: >"$serial"
	docker exec heph-e2e-qemu /e2e/run-vm.sh "$name" "$fw" "$mac" "$uuid" "$((BOOT_TIMEOUT + 10))" >"$out/$name.vm.log" 2>&1 &
	vm=$!

	started=$(date +%s)
	result=timeout
	while [ "$(($(date +%s) - started))" -lt "$BOOT_TIMEOUT" ]; do
		if [ "$uefi" = yes ]; then
			grep -q 'hephaestus: holding' "$serial" && { result=held; break; }
			grep -qE 'Could not boot|giving up' "$serial" && { result=failed; break; }
		else # SeaBIOS writes to VGA, so the server's own log is the evidence: a hold and a stored report
			since heph-e2e-hephaestus "$hm" >"$out/$name.poll.log"
			h=$(jn "$out/$name.poll.log" 'fromjson? | select(.msg=="request" and .path=="/boot" and .action=="hold")')
			d=$(jn "$out/$name.poll.log" 'fromjson? | select(.msg=="request" and .path=="/discover" and .outcome=="stored")')
			if ge "$h" 1 && ge "$d" 1; then result=held; break; fi
		fi
		kill -0 "$vm" 2>/dev/null || { result=exited; break; }
		sleep 2
	done
	elapsed=$(($(date +%s) - started))
	docker exec heph-e2e-qemu pkill -f qemu-system >/dev/null 2>&1 || true
	wait "$vm" || true
	rm -f "$out/$name.poll.log"

	since heph-e2e-hephaestus "$hm" >"$out/$name.hephaestus.log"
	if [ "$mode" = proxy ]; then since heph-e2e-caddy "$cm" >"$out/$name.caddy.log"; fi
	docker logs heph-e2e-dnsmasq >"$out/dnsmasq-$heph_mode.log" 2>&1 || true
	hlog=$out/$name.hephaestus.log

	if [ "$result" = held ]; then
		pass "$name: held after ${elapsed}s"
	else
		fail "$name: $result after ${elapsed}s (no hold within ${BOOT_TIMEOUT}s)"
		tail -n 15 "$serial" | sed 's/^/    serial| /'
	fi

	# The VM's address: the last DHCPACK dnsmasq sent to its MAC. No address means no DHCP, which fails the boot.
	ip=$(grep "DHCPACK" "$out/dnsmasq-$heph_mode.log" | grep -i " $mac" | awk '{for (i = 1; i <= NF; i++) if ($i ~ /^10\.99\.0\./) ip = $i} END {print ip}')
	check "$name: dnsmasq leased an address to $mac${ip:+ ($ip)}" test -n "$ip"

	# 1. proxyDHCP offered the expected boot file and TFTP sent it
	n=$(jn "$hlog" --arg mac "$mac" --arg f "$offered" 'fromjson? | select(.msg=="offering boot response" and .source==$mac and .boot_filename==$f)')
	check "$name: proxyDHCP offered $offered to $mac" ge "$n" 1
	n=$(jn "$hlog" --arg f "$sent" 'fromjson? | select(.msg=="file sent" and .filename==$f and .bytes>0)')
	check "$name: TFTP sent $sent" ge "$n" 1

	# 2. the HTTP legs. In proxy mode /boot and /discover arrive from Caddy with the client in client_ip, over plain HTTP.
	if [ "$mode" = proxy ]; then
		who='.remote_ip=="'$CADDY_IP'" and .client_ip==$ip'
		tlsval=false
	else
		who='.remote_ip==$ip'
		tlsval=true
	fi
	n=$(jn "$hlog" --arg ip "$ip" 'fromjson? | select(.msg=="request" and .path=="/start" and .status==200 and .tls==false and .remote_ip==$ip)')
	check "$name: /start 200 over plain HTTP (tls:false)" ge "$n" 1
	n=$(jn "$hlog" --arg ip "$ip" "fromjson? | select(.msg==\"request\" and .path==\"/boot\" and .status==200 and .tls==$tlsval and .decision_row==7 and .action==\"hold\" and $who)")
	check "$name: /boot 200 row 7 hold (tls:$tlsval at hephaestus)" ge "$n" 1
	n=$(jn "$hlog" --arg ip "$ip" "fromjson? | select(.msg==\"request\" and .path==\"/discover\" and .method==\"POST\" and .status==200 and .outcome==\"stored\" and .tls==$tlsval and $who)")
	check "$name: POST /discover 200 stored" ge "$n" 1

	# TLS must have completed, so no handshake errors. A log with no requests in it proves nothing, so it must have some.
	if [ "$mode" = proxy ]; then
		tlslog=$out/$name.caddy.log
		check "$name: Caddy logged the /boot request as TLS 1.3 (0x0304), status 200" \
			ge "$(jn "$tlslog" 'fromjson? | select(.msg=="handled request" and .status==200 and (.request.uri|startswith("/boot")) and .request.tls.version==772)')" 1
	else
		tlslog=$hlog
	fi
	check "$name: $(basename "$tlslog") is not empty" test -s "$tlslog"
	check "$name: zero TLS handshake error lines" sh -c '[ -s "$1" ] && ! grep -q "TLS handshake error" "$1"' _ "$tlslog"

	# 3. serial console (UEFI only; SeaBIOS has no serial output)
	if [ "$uefi" = yes ]; then
		check "$name: serial: https://$FQDN/discover... ok" sh -c '[ -s "$1" ] && grep -qF "https://$2/discover... ok" "$1"' _ "$serial" "$FQDN"
		check "$name: serial: hephaestus: holding: awaiting approval" sh -c '[ -s "$1" ] && grep -q "hephaestus: holding: awaiting approval" "$1"' _ "$serial"
	else
		printf 'NOTE %s: serial not asserted (SeaBIOS writes to VGA only); the server log above is the evidence\n' "$name"
	fi

	# 4. one discovered, non-junk machine row for the MAC, and the hold counter moved
	row=$(sqlite_q "SELECT m.state||'|'||m.junk||'|'||m.arch||'|'||m.smbios_uuid FROM machines m JOIN machine_macs mm ON mm.machine_id = m.id WHERE lower(mm.mac) = lower('$mac')")
	printf '%s\n' "$row" >"$out/$name.row.txt"
	check "$name: sqlite has exactly one row for $mac: discovered|0|$arch|$uuid" test "$row" = "discovered|0|$arch|$uuid"
	hold1=$(hold_count)
	check "$name: hephaestus_boot_decisions_total{action=\"hold\",decision_row=\"7\"} rose ($hold0 -> $hold1)" ge "$((hold1 - hold0))" 1
}

# --- main ---
step "clean up anything a killed run left behind"
remove_all
mkdir -p "$out" "$certs" build/ipxe/out

step "toolbox image"
docker build --platform "$PLATFORM" -q -t "$TOOLS" test/e2e/qemu >/dev/null

step "test CA and leaf certificate"
docker run --rm --user "$(id -u):$(id -g)" -v "$root/$certs:/certs" -v "$root/test/e2e:/e2e:ro" "$TOOLS" sh /e2e/gen-certs.sh >"$out/gen-certs.log" 2>&1

step "iPXE built to trust only the test CA"
cp "$certs/ca.pem" "$testca_pem"
"$TASK" ipxe:build:testca TEST_CA="$testca_pem" >"$out/ipxe-build.log" 2>&1 || { tail -n 30 "$out/ipxe-build.log"; exit 1; }
for f in undionly.kpxe snp-amd64.efi snp-arm64.efi roots.pem; do
	[ -s "$testca_dir/$f" ] || { echo "e2e: $testca_dir/$f missing after the testca build" >&2; exit 1; }
done

step "hephaestus:e2e from a temporary copy of the tracked tree, with the test-CA binaries overlaid"
# The repository tree and the production Dockerfile never hold test-CA binaries: the overlay lives only in this copy.
src=$(mktemp -d "${TMPDIR:-/tmp}/heph-e2e-src.XXXXXX")
git ls-files -z | tar --null -T - -cf - | tar -xf - -C "$src"
cp "$testca_dir"/* "$src/internal/boot/ipxe/bin/"
docker buildx build --platform "$PLATFORM" --load -q \
	--build-arg VERSION=e2e --build-arg COMMIT="$(git rev-parse --short HEAD)" --build-arg DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	-t "$IMAGE" "$src" >"$out/image-build.log" 2>&1 || { tail -n 30 "$out/image-build.log"; exit 1; }
rm -rf "${src:?}"
src=

step "network, dnsmasq, hephaestus (file mode)"
docker network create --driver bridge --subnet 10.99.0.0/24 --gateway "$GATEWAY" "$NET" >/dev/null
docker volume create "$VOL" >/dev/null
docker run -d --name heph-e2e-qemu --network "$NET" --ip "$QEMU_IP" --cap-add NET_ADMIN --device /dev/net/tun \
	-v "$root/test/e2e/qemu:/e2e:ro" -v "$root/$out:/out" "$TOOLS" sleep infinity >/dev/null
start_dnsmasq "$HEPH_IP"
start_hephaestus file
check "seed: unknown_action = hold" seed_hold

step "run-level checks (file mode)"
tls_checks file "$HEPH_IP"
capability_checks

boot ovmf ovmf 52:54:00:e2:e0:03 amd64 snp.efi snp.efi file
boot seabios seabios 52:54:00:e2:e0:04 amd64 tftp://$HEPH_IP/undionly.kpxe undionly.kpxe file
boot aarch64 aarch64 52:54:00:e2:e0:05 arm64 snp-arm64.efi snp-arm64.efi file

step "proxy mode: Caddy at $CADDY_IP terminates TLS, hephaestus serves plain HTTP behind it"
save_logs
docker rm -f heph-e2e-hephaestus heph-e2e-dnsmasq >/dev/null
heph_mode=proxy
start_dnsmasq "$CADDY_IP"
start_hephaestus proxy
docker run -d --name heph-e2e-caddy --network "$NET" --ip "$CADDY_IP" -v "$root/test/e2e:/caddy:ro" -v "$root/$certs:/certs:ro" \
	"$CADDY_IMAGE" caddy run --config /caddy/caddy.json >/dev/null
wait_for "Caddy listening" 30 sh -c 'docker logs heph-e2e-caddy 2>&1 | grep -q "serving initial configuration"'
tls_checks proxy "$CADDY_IP"
code=$(box curl -s -o /dev/null -w '%{http_code}' "http://$HEPH_IP:8443/boot" || true)
check "proxy: a direct request from a non-proxy peer gets 403 (got ${code:-none})" test "$code" = 403
boot ovmf-proxy ovmf 52:54:00:e2:e0:06 amd64 snp.efi snp.efi proxy

step "sqlite dump"
sqlite_q .dump >"$out/sqlite.dump.sql"
check "sqlite dump holds the four machines" test "$(grep -c 'INSERT INTO machines' "$out/sqlite.dump.sql")" -eq 4

[ "$fails" -eq 0 ]
