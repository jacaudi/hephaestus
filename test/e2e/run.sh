#!/bin/sh
# QEMU end-to-end test (design §7 criterion 6). Run it as `task e2e`; it needs a reachable Docker daemon
# (colima: set DOCKER_HOST) and nothing else on the host (see test/e2e/README.md). Boots amd64 UEFI, amd64 BIOS
# and arm64 UEFI VMs through hephaestus in file mode, then the M2 utility phase against a local TLS 1.3 fake upstream
# (three cases on the same firmwares), then the M3 install phase (a fixture cluster imported from a git host, approved,
# and one VM per firmware, amd64 UEFI, amd64 BIOS and arm64 UEFI, approved into it and booted to its install script,
# against the upstreams program's fake factory), then amd64 UEFI again with hephaestus behind Caddy.
# Writes every log to build/e2e/<UTC timestamp>/, with events.jsonl (one event per phase start and end, check and run
# start and end; test/e2e/events.schema.json), run.log (everything it printed) and manifest.json (every file's size and
# sha256). The console shows the same events through test/e2e/lib/events.sh: a header per phase, a PASS, FAIL or NOTE
# line per check, a subtotal per phase that has checks, and a summary. On any exit it removes the heph-e2e-*
# containers, network and volume, and the temporary source copy, fake-upstream files, certificates and test-CA build
# outputs that this run created. The names, network and subnet are fixed: one run at a time.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd -P)
cd "$root"
# shellcheck source=SCRIPTDIR/lib/events.sh
. "$root/test/e2e/lib/events.sh"

# --- pins and topology (the spike's, design §7) ---
# Caddy is pinned by the digest of its multi-arch index (caddy:2 on 2026-11-15); the toolbox pins its own base.
CADDY_IMAGE=docker.io/library/caddy:2@sha256:4e2425f6009577e7c651010f16679c28f7d12df3e7b1914f941a7a3370746b3c
# The upstreams program runs with `go run` in the image the production Dockerfile builds with (it carries git).
GO_IMAGE=docker.io/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9
TOOLS=hephaestus-e2e-qemu:local
FIXTURE=hephaestus-e2e-fixture:local # test/e2e/fixture: the files the fake upstream serves
PIN_IMAGE=hephaestus-catalogpin:e2e
IMAGE=hephaestus:e2e
NET=heph-e2e
VOL=heph-e2e-data
GATEWAY=10.99.0.1
DNS_IP=10.99.0.2
HEPH_IP=10.99.0.10
CADDY_IP=10.99.0.11
UPSTREAM_IP=10.99.0.12
UPSTREAMS_IP=10.99.0.13 # test/e2e/upstreams: the fake factory, its assets host and the git host
QEMU_IP=10.99.0.20
FQDN=hephaestus.e2e.test
UPSTREAM_FQDN=upstream.e2e.test
FACTORY_FQDN=factory.e2e.test # test/e2e/overlay/base.go.in points hephaestus here
ASSETS_FQDN=assets.factory.e2e.test
GIT_FQDN=git.e2e.test
# The fixture cluster (test/e2e/fixture-cluster/lab): its plain schematic and Talos version, and the test data key.
TALOS_SCHEMATIC=376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba
TALOS_VERSION=v1.12.11
DATA_KEY=AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=
BOOT_TIMEOUT=120 # seconds per boot
# A utility boot runs a TCG VM through a kernel and, for e2e-live, a second-stage fetch: a fail line takes up to about
# 2 minutes on its own (30 s for the interface, 30 s for DHCP, 60 s for the fetch).
UTILITY_TIMEOUT=300
# An install boot holds at row 8 until the Talos assets are fetched; the hold script asks /boot again every 30 s.
INSTALL_TIMEOUT=300
TASK=${TASK_EXE:-task}

# --- the run directory and the capture ---
# The run captures itself: from capture_start on (test/e2e/lib/events.sh), everything it prints, every docker build
# and pull included, also goes to $out/run.log. It stays the process that was started, so SIGINT and SIGTERM reach
# the traps below as they would without the capture. The cleanup trap ends the capture, so run.log is complete, then
# writes $out/manifest.json and exits with the run's status. Colour is decided before capture_start, while stdout may
# still be the terminal.
ts=$(date -u +%Y%m%dT%H%M%SZ)
out=build/e2e/$ts
commit=$(git rev-parse HEAD)

# write_manifest: $out/manifest.json lists every file under $out but itself, with its size and sha256, and the run id
# and commit. It runs in the toolbox, which has sha256sum and jq; the host is promised nothing but docker.
write_manifest() {
	docker run --rm -v "$root/$out:/out:ro" "$TOOLS" sh -c '
		set -eu
		cd /out
		find . -type f ! -path ./manifest.json | sed "s|^\./||" | LC_ALL=C sort >/tmp/files
		while IFS= read -r f; do
			size=$(stat -c %s "$f")
			sum=$(sha256sum <"$f")
			printf "%s\t%s\t%s\n" "$f" "$size" "${sum%% *}"
		done </tmp/files >/tmp/rows
		jq -R -n --arg run_id "$1" --arg git_commit "$2" "$3" </tmp/rows' _ "$ts" "$commit" \
		'{run_id: $run_id, git_commit: $git_commit, files: [inputs | split("\t") | {path: .[0], size: (.[1] | tonumber), sha256: .[2]}]}' \
		>"$out/manifest.json"
}

if ! docker info >/dev/null 2>&1; then
	echo "e2e: cannot reach a Docker daemon. With colima: export DOCKER_HOST=unix://\$HOME/.colima/default/docker.sock" >&2
	exit 1
fi
mkdir -p "$out"
colour=0
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then # https://no-color.org
	colour=1
fi
capture_start "$out/run.log"

# --- the run ---
certs=build/e2e/run/certs
tree=build/e2e/run/tree         # the temporary copy of the tracked tree; the pin container bind-mounts it
upstream=build/e2e/run/upstream # the fixture files the fake upstream serves
testca_pem=build/ipxe/out/e2e-ca.pem
testca_dir=build/ipxe/out/testca
own_pem=no # whether this run created the test-CA files under build/ipxe/out, which it then removes
own_testca=no
heph_mode='file'
# The run's record and console: step, pass, fail, note and check come from test/e2e/lib/events.sh.
ev_init "$out" "$ts" "$commit" file+proxy "$colour"

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
# hold_count prints the hold counter, or nothing and fails when the metrics cannot be read (so a failed scrape is never a 0).
hold_count() {
	m=$(sidecar curl -fsS http://127.0.0.1:9090/metrics) || return 1
	printf '%s\n' "$m" |
		awk '/^hephaestus_boot_decisions_total\{/ && /action="hold"/ && /decision_row="7"/ {s += $NF} END {print s + 0}'
}
# rose <before> <after>: both counters were read and the second is at least one higher.
rose() { [ -n "$1" ] && [ -n "$2" ] && [ "$(($2 - $1))" -ge 1 ]; }
# one_more <before> <after>: both were read and the second is exactly one higher.
one_more() { [ -n "$1" ] && [ -n "$2" ] && [ "$(($2 - $1))" -eq 1 ]; }
# The admin CLI inside the hephaestus container. heph_to <file> <args...> keeps its output in a file.
heph() { docker exec heph-e2e-hephaestus /hephaestus "$@"; }
heph_to() {
	f=$1
	shift
	heph "$@" >"$f" 2>&1
}
# boots_count <entry> prints the utility boots counter for the entry, or nothing and fails when the metrics cannot be read.
boots_count() {
	m=$(sidecar curl -fsS http://127.0.0.1:9090/metrics) || return 1
	printf '%s\n' "$m" |
		awk -v entry="entry=\"$1\"" '/^hephaestus_utility_boots_total\{/ && index($0, entry) {s += $NF} END {print s + 0}'
}
# flag_cleared <mac>: the machine's boot_next is NULL (a missing machine prints nothing, so it fails).
flag_cleared() {
	[ "$(sqlite_q "SELECT m.boot_next IS NULL FROM machines m JOIN machine_macs mm ON mm.machine_id = m.id WHERE lower(mm.mac) = lower('$1')")" = 1 ]
}
# audit_count <mac> <reason> prints how many boot_next.cleared audit events with that reason the machine has.
audit_count() {
	sqlite_q "SELECT count(DISTINCT a.id) FROM audit_events a JOIN machine_macs mm ON mm.machine_id = a.machine_id WHERE lower(mm.mac) = lower('$1') AND a.action = 'boot_next.cleared' AND json_extract(a.detail, '\$.reason') = '$2'"
}
# lease_re <dnsmasq log> <mac> prints an anchored regex of every address dnsmasq acked to the MAC (the first stage and a
# second stage may hold different leases), or nothing when there is none.
lease_re() {
	grep "DHCPACK" "$1" | grep -i " $2" |
		awk '{for (i = 1; i <= NF; i++) if ($i ~ /^10\.99\.0\./) a[$i] = 1} END {for (k in a) s = s (s == "" ? "" : "|") k; if (s != "") print "^(" s ")$"}' |
		sed 's/\./\\./g'
}
# utility_rows_ready: `utility list --json` shows all four e2e rows ready.
utility_rows_ready() { heph utility list --json | box jq -e 'length == 4 and all(.[]; .state == "ready")'; }
mark() { docker logs "$1" 2>&1 | wc -l | tr -d ' '; }
since() { docker logs "$1" 2>&1 | tail -n +$(($2 + 1)); }

save_logs() {
	for c in dnsmasq hephaestus caddy upstream upstreams qemu; do
		if docker inspect "heph-e2e-$c" >/dev/null 2>&1; then
			case $c in hephaestus | dnsmasq) label=$c-$heph_mode ;; *) label=$c ;; esac
			docker logs "heph-e2e-$c" >"$out/$label.log" 2>&1 || true
		fi
	done
}

# events_valid: events.jsonl so far matches events.schema.json and counts its events from 1, by the upstreams program's
# validate-events in the Go image: inside the upstreams container while it runs (its build cache is warm), else in a
# new container. The check this answers, its phase's end and run_end come after; TestEvents_* in upstreams/main_test.go
# validate those.
events_valid() {
	set -- go run ./test/e2e/upstreams validate-events test/e2e/events.schema.json "$out/events.jsonl"
	if [ "$(docker inspect -f '{{.State.Running}}' heph-e2e-upstreams 2>/dev/null)" = true ]; then
		docker exec heph-e2e-upstreams "$@"
	else
		docker run --rm -v "$root:/src:ro" -w /src -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local "$GO_IMAGE" "$@"
	fi >"$out/events-validation.txt" 2>&1
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	set +e
	step cleanup 'Clean up: save the logs, validate the event record, remove the containers and temporary files'
	save_logs
	check -e "$out/events-validation.txt" 'events.jsonl matches events.schema.json' events_valid
	for c in $(docker ps -aq --filter 'name=^heph-e2e-'); do docker rm -f "$c" >/dev/null 2>&1; done
	docker network rm "$NET" >/dev/null 2>&1
	docker volume rm "$VOL" >/dev/null 2>&1
	rm -rf "${tree:?}" "${upstream:?}"
	rm -rf "${certs:?}"
	[ "$own_pem" = yes ] && rm -f "${testca_pem:?}"
	[ "$own_testca" = yes ] && rm -rf "${testca_dir:?}"
	if [ "$status" -eq 0 ] && ! ev_all_passed; then status=1; fi
	ev_end "$status"
	capture_finish # run.log is complete from here on
	if ! merr=$(write_manifest 2>&1); then
		rm -f "$out/manifest.json"
		printf 'e2e: could not write %s/manifest.json\n%s\n' "$out" "$merr" | tee -a "$out/run.log" >&2
		if [ "$status" -eq 0 ]; then
			status=1 # a run whose record is incomplete has not passed
		fi
	fi
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
		-v "$VOL":/var/lib/hephaestus -v "$root/$certs:/certs:ro" \
		--add-host "$UPSTREAM_FQDN:$UPSTREAM_IP" -e SSL_CERT_FILE=/certs/ca.pem \
		--add-host "$FACTORY_FQDN:$UPSTREAMS_IP" --add-host "$ASSETS_FQDN:$UPSTREAMS_IP" --add-host "$GIT_FQDN:$UPSTREAMS_IP" \
		-e HEPHAESTUS_DATA_KEY="$DATA_KEY" -e HEPHAESTUS_OP_TOKEN=e2e-dummy \
		-e HEPHAESTUS_FQDN="$FQDN" -e HEPHAESTUS_PROVISIONING_INTERFACE=eth0 \
		-e HEPHAESTUS_PROVISIONING_ADDRESS="$HEPH_IP" -e HEPHAESTUS_PROVISIONING_SUBNET=10.99.0.0/24 \
		-e HEPHAESTUS_TLS_MODE="$heph_mode" -e HEPHAESTUS_LOG_LEVEL=debug
	case $heph_mode in
	file) set -- "$@" -e HEPHAESTUS_TLS_CERT_FILE=/certs/fullchain.pem -e HEPHAESTUS_TLS_KEY_FILE=/certs/privkey.pem ;;
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

# unknown_is_hold: the unknown_action setting is hold (it defaults to local, the row 7 exit).
unknown_is_hold() { [ "$(sqlite_q "SELECT value FROM settings WHERE key='unknown_action'")" = hold ]; }

# The upstreams program (test/e2e/upstreams), built from the working tree with `go run`. Its work directory is a tmpfs:
# it commits the fixture tree to /work/lab.git at start.
start_upstreams() {
	docker run -d --name heph-e2e-upstreams --network "$NET" --ip "$UPSTREAMS_IP" \
		-v "$root:/src:ro" -v "$root/$certs:/certs:ro" --tmpfs /work -w /src \
		-e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local -e UPSTREAMS_ADDR=:443 \
		-e UPSTREAMS_FACTORY_CERT_FILE=/certs/factory-fullchain.pem -e UPSTREAMS_FACTORY_KEY_FILE=/certs/factory-privkey.pem \
		-e UPSTREAMS_ASSETS_CERT_FILE=/certs/assets-fullchain.pem -e UPSTREAMS_ASSETS_KEY_FILE=/certs/assets-privkey.pem \
		-e UPSTREAMS_GIT_CERT_FILE=/certs/git-fullchain.pem -e UPSTREAMS_GIT_KEY_FILE=/certs/git-privkey.pem \
		-e UPSTREAMS_FIXTURE_DIR=/src/test/e2e/fixture-cluster -e UPSTREAMS_WORK_DIR=/work \
		"$GO_IMAGE" go run ./test/e2e/upstreams >/dev/null
	# The first run compiles the program in a fresh container.
	wait_for "the upstreams program listening" 300 sh -c 'docker logs heph-e2e-upstreams 2>&1 | grep -q "\"msg\":\"listening\""'
}

# upstreams_tls_checks: each name the program serves completes a TLS 1.3 handshake whose certificate verifies for that
# name against the test CA (the program picks the leaf by SNI), and TLS 1.2 is refused.
upstreams_tls_checks() {
	for h in "$FACTORY_FQDN" "$ASSETS_FQDN" "$GIT_FQDN"; do
		r=$(docker run --rm --network "$NET" -v "$root/$certs:/certs:ro" "$TOOLS" \
			sh -c "openssl s_client -connect $UPSTREAMS_IP:443 -servername $h -verify_hostname $h -CAfile /certs/ca.pem -tls1_3 </dev/null 2>&1" || true)
		printf '%s\n' "$r" >"$out/tls13-upstreams-$h.txt"
		check -e "$out/tls13-upstreams-$h.txt" "TLS 1.3 to $h verifies against the test CA for that name" \
			sh -c 'grep -q "Verify return code: 0 (ok)" "$1" && grep -q "TLSv1.3" "$1"' _ "$out/tls13-upstreams-$h.txt"
	done
	r=$(docker run --rm --network "$NET" -v "$root/$certs:/certs:ro" "$TOOLS" \
		sh -c "openssl s_client -connect $UPSTREAMS_IP:443 -servername $FACTORY_FQDN -CAfile /certs/ca.pem -tls1_2 </dev/null 2>&1" || true)
	printf '%s\n' "$r" >"$out/tls12-upstreams.txt"
	check -e "$out/tls12-upstreams.txt" "openssl s_client -tls1_2 gets a protocol-version alert" grep -qi "alert protocol version" "$out/tls12-upstreams.txt"
}

# --- run-level checks ---
# tls_checks <label> <ip> <container>: TLS 1.2 is refused, and TLS 1.3 completes against the test CA with no session
# ticket (iPXE 9261ebe hashes a NewSessionTicket sent before its Finished into the transcript and fails the handshake).
# <container> is the TLS terminator whose log must record the refused TLS 1.2 probe as exactly one handshake error. That
# positive control is what lets the boots' "zero TLS handshake error lines" check fail: a logger that cannot print the
# line would make it pass for any server.
tls_checks() {
	label=$1 ip=$2 terminator=$3
	probe="printf 'GET /e2e-probe HTTP/1.1\r\nHost: $FQDN\r\nConnection: close\r\n\r\n' | openssl s_client -ign_eof -connect $ip:443 -servername $FQDN -CAfile /certs/ca.pem"
	tls12=$(docker run --rm --network "$NET" -v "$root/$certs:/certs:ro" "$TOOLS" sh -c "$probe -tls1_2 2>&1" || true)
	printf '%s\n' "$tls12" >"$out/tls12-$label.txt"
	check -e "$out/tls12-$label.txt" "openssl s_client -tls1_2 gets a protocol-version alert" sh -c 'grep -qi "alert protocol version" "$1"' _ "$out/tls12-$label.txt"
	tls13=$(docker run --rm --network "$NET" -v "$root/$certs:/certs:ro" "$TOOLS" sh -c "$probe -tls1_3 2>&1" || true)
	printf '%s\n' "$tls13" >"$out/tls13-$label.txt"
	check -e "$out/tls13-$label.txt" "TLS 1.3 handshake verifies against the test CA" sh -c 'grep -q "Verify return code: 0 (ok)" "$1" && grep -q "TLSv1.3" "$1"' _ "$out/tls13-$label.txt"
	# -ign_eof keeps s_client reading until the server closes: without it s_client quits at stdin EOF before it
	# reads a post-handshake ticket, and this check would pass whether or not one was sent. The 404 proves it read.
	check -e "$out/tls13-$label.txt" "the probe read the server's response after the handshake (HTTP 404)" grep -q '^HTTP/1.1 404' "$out/tls13-$label.txt"
	check -e "$out/tls13-$label.txt" "no NewSessionTicket sent after the handshake" sh -c '! grep -qi "New Session Ticket" "$1"' _ "$out/tls13-$label.txt"
	docker logs "$terminator" >"$out/$terminator-probe.log" 2>&1 || true
	n=$(grep -c 'TLS handshake error' "$out/$terminator-probe.log" || true)
	check -d "${n:-0}" -e "$out/$terminator-probe.log" \
		"positive control: $terminator logged exactly one TLS handshake error, for the TLS 1.2 probe" test "${n:-0}" -eq 1
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
	check -e "$out/cap-drop-all.txt" "--cap-drop ALL without the cap-add fails to exec (file capability)" sh -c '[ "$1" = yes ] && grep -q "operation not permitted" "$2"' _ "$rc_ok" "$out/cap-drop-all.txt"

	docker run --rm --network container:heph-e2e-hephaestus "$TOOLS" ss -H -lnut >"$out/listeners.txt" 2>&1 || true
	for p in "udp 67" "udp 69" "udp 4011" "tcp 80" "tcp 443"; do
		# shellcheck disable=SC2086 # deliberate word split into protocol and port
		set -- $p
		check -e "$out/listeners.txt" "CAP_NET_BIND_SERVICE: hephaestus listens on $1 $2" listens "$1" "$2" "$out/listeners.txt"
	done
	docker run --rm --pid container:heph-e2e-hephaestus --cap-add SYS_PTRACE "$TOOLS" grep -E '^(Uid|Gid|CapEff|NoNewPrivs):' /proc/1/status >"$out/proc-status.txt" 2>&1 || true
	check -e "$out/proc-status.txt" "hephaestus runs as UID 65532, not root" runs_as_65532 "$out/proc-status.txt"
}

# --- one boot ---
# boot <name> <firmware> <mac> <arch> <offered boot file> <sent file> <file|proxy>
boot() {
	name=$1 fw=$2 mac=$3 arch=$4 offered=$5 sent=$6 mode=$7
	uuid=e2e00000-0000-4000-8000-$(printf '%s' "$mac" | tr -d :)
	serial=$out/$name.serial.log
	uefi=yes
	[ "$fw" = seabios ] && uefi=no
	if [ "$uefi" = yes ]; then firmware=UEFI; else firmware=BIOS; fi
	step "boot-$name" "Boot $name" arch="$arch" firmware=$firmware mode="$mode"
	hm=$(mark heph-e2e-hephaestus)
	[ "$mode" = proxy ] && cm=$(mark heph-e2e-caddy)
	hold0=$(hold_count) || hold0=
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
		pass -d "after ${elapsed}s" -e "$serial" -e "$hlog" "held within ${BOOT_TIMEOUT}s"
	else
		fail -d "$result after ${elapsed}s" -e "$serial" -e "$hlog" "held within ${BOOT_TIMEOUT}s"
		tail -n 15 "$serial" | sed 's/^/        serial| /'
	fi

	# The VM's address: the last DHCPACK dnsmasq sent to its MAC. No address means no DHCP, which fails the boot.
	ip=$(grep "DHCPACK" "$out/dnsmasq-$heph_mode.log" | grep -i " $mac" | awk '{for (i = 1; i <= NF; i++) if ($i ~ /^10\.99\.0\./) ip = $i} END {print ip}')
	check -d "${ip:-none}" -e "$out/dnsmasq-$heph_mode.log" "dnsmasq leased an address to $mac" test -n "$ip"

	# 1. proxyDHCP offered the expected boot file and TFTP sent it
	n=$(jn "$hlog" --arg mac "$mac" --arg f "$offered" 'fromjson? | select(.msg=="offering boot response" and .source==$mac and .boot_filename==$f)')
	check -e "$hlog" "proxyDHCP offered $offered to $mac" ge "$n" 1
	n=$(jn "$hlog" --arg f "$sent" 'fromjson? | select(.msg=="file sent" and .filename==$f and .bytes>0)')
	check -e "$hlog" "TFTP sent $sent" ge "$n" 1

	# 2. the HTTP legs. In proxy mode /boot and /discover arrive from Caddy with the client in client_ip, over plain HTTP.
	if [ "$mode" = proxy ]; then
		who='.remote_ip=="'$CADDY_IP'" and .client_ip==$ip'
		tlsval=false
	else
		who='.remote_ip==$ip'
		tlsval=true
	fi
	n=$(jn "$hlog" --arg ip "$ip" 'fromjson? | select(.msg=="request" and .path=="/start" and .status==200 and .tls==false and .remote_ip==$ip)')
	check -e "$hlog" "/start 200 over plain HTTP (tls:false)" ge "$n" 1
	n=$(jn "$hlog" --arg ip "$ip" "fromjson? | select(.msg==\"request\" and .path==\"/boot\" and .status==200 and .tls==$tlsval and .decision_row==7 and .action==\"hold\" and $who)")
	check -e "$hlog" "/boot 200 row 7 hold (tls:$tlsval at hephaestus)" ge "$n" 1
	n=$(jn "$hlog" --arg ip "$ip" "fromjson? | select(.msg==\"request\" and .path==\"/discover\" and .method==\"POST\" and .status==200 and .outcome==\"stored\" and .tls==$tlsval and $who)")
	check -e "$hlog" "POST /discover 200 stored" ge "$n" 1

	# TLS must have completed, so no handshake errors. A log with no requests in it proves nothing, so it must have some.
	if [ "$mode" = proxy ]; then
		tlslog=$out/$name.caddy.log
		check -e "$tlslog" "Caddy logged the /boot request as TLS 1.3 (0x0304), status 200" \
			ge "$(jn "$tlslog" 'fromjson? | select(.msg=="handled request" and .status==200 and (.request.uri|startswith("/boot")) and .request.tls.version==772)')" 1
	else
		tlslog=$hlog
	fi
	check -e "$tlslog" "$(basename "$tlslog") is not empty" test -s "$tlslog"
	check -e "$tlslog" "zero TLS handshake error lines" sh -c '[ -s "$1" ] && ! grep -q "TLS handshake error" "$1"' _ "$tlslog"

	# 3. serial console (UEFI only; SeaBIOS has no serial output)
	if [ "$uefi" = yes ]; then
		check -e "$serial" "serial: https://$FQDN/discover... ok" sh -c '[ -s "$1" ] && grep -qF "https://$2/discover... ok" "$1"' _ "$serial" "$FQDN"
		check -e "$serial" "serial: hephaestus: holding: awaiting approval" sh -c '[ -s "$1" ] && grep -q "hephaestus: holding: awaiting approval" "$1"' _ "$serial"
	else
		note -e "$hlog" 'serial not asserted (SeaBIOS writes to VGA only); the server log above is the evidence'
	fi

	# 4. one discovered, non-junk machine row for the MAC, and the hold counter moved
	row=$(sqlite_q "SELECT m.state||'|'||m.junk||'|'||m.arch||'|'||m.smbios_uuid FROM machines m JOIN machine_macs mm ON mm.machine_id = m.id WHERE lower(mm.mac) = lower('$mac')")
	printf '%s\n' "$row" >"$out/$name.row.txt"
	check -e "$out/$name.row.txt" "sqlite has exactly one row for $mac: discovered|0|$arch|$uuid" test "$row" = "discovered|0|$arch|$uuid"
	hold1=$(hold_count) || hold1=
	check -d "${hold0:-unread} -> ${hold1:-unread}" \
		"hephaestus_boot_decisions_total{action=\"hold\",decision_row=\"7\"} rose" rose "$hold0" "$hold1"
}

# --- the utility phase (M2, design 5.9) ---
# utility_setup: asks serve to fetch both e2e entries from the fake upstream and waits for the four rows to be ready.
utility_setup() {
	um=$(mark heph-e2e-hephaestus)
	check -e "$out/utility-fetch-e2e-shell.txt" "utility fetch e2e-shell exits 0" heph_to "$out/utility-fetch-e2e-shell.txt" utility fetch e2e-shell
	check -e "$out/utility-fetch-e2e-live.txt" "utility fetch e2e-live exits 0" heph_to "$out/utility-fetch-e2e-live.txt" utility fetch e2e-live
	check -e "$out/utility-list.json" "utility list --json shows all four e2e rows ready within 120 polls" wait_for 'the four e2e rows ready' 120 utility_rows_ready
	heph utility list --json >"$out/utility-list.json" 2>&1 || true
	since heph-e2e-hephaestus "$um" >"$out/utility-setup.hephaestus.log"
	n=$(jn "$out/utility-setup.hephaestus.log" 'fromjson? | select(.msg=="utility fetch" and .to=="ready")')
	check -d "${n:-0}" -e "$out/utility-setup.hephaestus.log" "hephaestus logged four utility fetch lines with to=ready" test "${n:-0}" -eq 4
}

# utility_boot <name> <firmware> <mac> <arch> <entry> <kind> [<file>...]: sets boot-next <entry> on <mac>, boots the VM
# once and judges the outcome. <file>... are the /u/ paths the boot must fetch from the entry, the last being the second
# stage of a live boot. Kinds:
#   shell        a UEFI VM chains the entry's EFI binary; the serial console shows the UEFI Shell.
#   live         a kernel and initrd boot and the initrd fetches the squashfs, printing a marker on the serial console
#                (UEFI) or leaving the squashfs request in the server log (BIOS has no serial output).
#   unsupported  the entry has no build for the VM's firmware: row 3 clears the flag with `unsupported` and the VM holds.
# Gate-2 BL-2 reads the outcomes. A shell /u/ 200 with no UEFI Shell on serial is a failure of the chain mechanism. A live
# boot whose kernel and initrd return 200 but that leaves no second-stage request and no marker is a failure of the
# kernel+initrd+boot mechanism. A `fail <reason>` marker means the kernel and initrd ran: a fixture or harness failure.
utility_boot() {
	name=$1 fw=$2 mac=$3 arch=$4 entry=$5 kind=$6
	shift 6
	uuid=e2e00000-0000-4000-8000-$(printf '%s' "$mac" | tr -d :)
	serial=$out/$name.serial.log
	st=$out/$name.serial.txt # the serial log without the carriage returns the UART adds
	uefi=yes
	[ "$fw" = seabios ] && uefi=no
	base=/u/$entry/$arch/1
	macdash=$(printf '%s' "$mac" | tr : -)
	reason=served
	[ "$kind" = unsupported ] && reason=unsupported
	if [ "$uefi" = yes ]; then firmware=UEFI; else firmware=BIOS; fi
	step "utility-$name" "Utility boot $name" arch="$arch" firmware=$firmware entry="$entry" kind="$kind"

	cleared0=$(audit_count "$mac" "$reason") || cleared0=
	boots0=$(boots_count "$entry") || boots0=
	hold0=$(hold_count) || hold0=
	if heph_to "$out/$name.boot-next.txt" boot-next set "$mac" "$entry"; then
		pass -e "$out/$name.boot-next.txt" "boot-next set $mac $entry exits 0"
	else
		fail -e "$out/$name.boot-next.txt" "boot-next set $mac $entry exits 0"
		sed 's/^/        boot-next| /' "$out/$name.boot-next.txt"
		return 0
	fi

	hm=$(mark heph-e2e-hephaestus)
	: >"$serial"
	: >"$st"
	docker exec heph-e2e-qemu /e2e/run-vm.sh "$name" "$fw" "$mac" "$uuid" "$((UTILITY_TIMEOUT + 10))" >"$out/$name.vm.log" 2>&1 &
	vm=$!

	started=$(date +%s)
	result=timeout
	while [ "$(($(date +%s) - started))" -lt "$UTILITY_TIMEOUT" ]; do
		tr -d '\r' <"$serial" >"$st"
		since heph-e2e-hephaestus "$hm" >"$out/$name.poll.log"
		case $kind-$uefi in
		shell-yes) grep -qF 'UEFI Interactive Shell' "$st" && { result=reached; break; } ;;
		live-yes) grep -qF 'HEPHAESTUS-E2E-SECOND-STAGE' "$st" && { result=reached; break; } ;;
		live-no) # the last <file> is the second stage: its request in the server log is the BIOS evidence
			for second; do :; done
			ge "$(jn "$out/$name.poll.log" --arg p "$base/$second" 'fromjson? | select(.msg=="request" and .path==$p and .status==200)')" 1 && { result=reached; break; } ;;
		unsupported-*) # held means a /boot hold plus the hold script's POST /discover, as in boot()
			b=$(jn "$out/$name.poll.log" 'fromjson? | select(.msg=="request" and .path=="/boot" and .boot_next_clear=="unsupported")')
			d=$(jn "$out/$name.poll.log" 'fromjson? | select(.msg=="request" and .path=="/discover" and .method=="POST" and .status==200)')
			ge "$b" 1 && ge "$d" 1 && { result=reached; break; } ;;
		esac
		if [ "$uefi" = yes ] && grep -qE 'Could not boot|giving up' "$st"; then result=failed; break; fi
		kill -0 "$vm" 2>/dev/null || { result=exited; break; }
		sleep 2
	done
	elapsed=$(($(date +%s) - started))
	docker exec heph-e2e-qemu pkill -f qemu-system >/dev/null 2>&1 || true
	wait "$vm" || true
	rm -f "$out/$name.poll.log"
	tr -d '\r' <"$serial" >"$st"

	since heph-e2e-hephaestus "$hm" >"$out/$name.hephaestus.log"
	docker logs heph-e2e-dnsmasq >"$out/dnsmasq-$heph_mode.log" 2>&1 || true
	hlog=$out/$name.hephaestus.log

	case $kind-$uefi in
	shell-*) what='the UEFI Shell on serial' ;;
	live-yes) what='a second-stage marker on serial' ;;
	live-no) what='the second-stage request in the server log' ;;
	unsupported-*) what='the unsupported clear and the hold'"'"'s POST /discover in the server log' ;;
	esac
	if [ "$result" = reached ]; then
		pass -d "after ${elapsed}s" -e "$st" -e "$hlog" "reached $what within ${UTILITY_TIMEOUT}s"
	else
		fail -d "$result after ${elapsed}s" -e "$st" -e "$hlog" "reached $what within ${UTILITY_TIMEOUT}s"
		tail -n 15 "$st" | sed 's/^/        serial| /'
	fi

	re=$(lease_re "$out/dnsmasq-$heph_mode.log" "$mac")
	check -e "$out/dnsmasq-$heph_mode.log" "dnsmasq leased an address to $mac" test -n "$re"
	[ -n "$re" ] || re='^no-lease$' # an empty pattern would match every address
	check -e "$hlog" "$(basename "$hlog") is not empty" test -s "$hlog"

	case $kind in
	unsupported)
		n=$(jn "$hlog" --arg ip "$re" 'fromjson? | select(.msg=="request" and .path=="/boot" and .status==200 and .decision_row==7 and .action=="hold" and .boot_next_clear=="unsupported" and (.remote_ip|test($ip)))')
		check -e "$hlog" "/boot 200 row 7 hold with boot_next_clear unsupported" ge "$n" 1
		n=$(jn "$hlog" --arg ip "$re" 'fromjson? | select(.msg=="request" and .path=="/discover" and .method=="POST" and .status==200 and (.remote_ip|test($ip)))')
		check -e "$hlog" "POST /discover 200 (the hold script ran)" ge "$n" 1
		n=$(jn "$hlog" 'fromjson? | select(.msg=="request" and (.path|startswith("/u/")))')
		check -d "${n:-unread}" -e "$hlog" "no /u/ request, so nothing was served" sh -c '[ -s "$1" ] && [ "$2" = 0 ]' _ "$hlog" "$n"
		hold1=$(hold_count) || hold1=
		check -d "${hold0:-unread} -> ${hold1:-unread}" \
			"hephaestus_boot_decisions_total{action=\"hold\",decision_row=\"7\"} rose" rose "$hold0" "$hold1"
		boots1=$(boots_count "$entry") || boots1=
		check -d "${boots0:-unread} -> ${boots1:-unread}" \
			"hephaestus_utility_boots_total{entry=\"$entry\"} did not move" sh -c '[ -n "$1" ] && [ "$1" = "$2" ]' _ "$boots0" "$boots1"
		;;
	*)
		n=$(jn "$hlog" --arg ip "$re" --arg e "$entry" 'fromjson? | select(.msg=="request" and .path=="/boot" and .status==200 and .decision_row==3 and .action=="utility" and .utility==$e and .boot_next_clear=="served" and (.remote_ip|test($ip)))')
		check -e "$hlog" "/boot 200 row 3, action utility, utility $entry, boot_next_clear served" ge "$n" 1
		for f in "$@"; do
			n=$(jn "$hlog" --arg ip "$re" --arg p "$base/$f" 'fromjson? | select(.msg=="request" and .path==$p and .status==200 and .tls==false and (.remote_ip|test($ip)))')
			check -e "$hlog" "GET $base/$f 200 from the VM over plain HTTP" ge "$n" 1
		done
		if [ "$uefi" = yes ]; then
			case $kind in
			shell) check -e "$st" "serial: UEFI Interactive Shell" sh -c '[ -s "$1" ] && grep -qF "UEFI Interactive Shell" "$1"' _ "$st" ;;
			live)
				sum=$(tr -d ' \t\r\n' <"$root/$upstream/squashfs.sha256")
				check "the fixture's squashfs.sha256 is one 64-digit hash" sh -c 'printf "%s" "$1" | grep -Eq "^[0-9a-f]{64}\$"' _ "$sum"
				check -e "$st" "serial: HEPHAESTUS-E2E-SECOND-STAGE ok bootif=01-$macdash sha256=<squashfs.sha256>" \
					sh -c '[ -s "$1" ] && grep -qF "$2" "$1"' _ "$st" "HEPHAESTUS-E2E-SECOND-STAGE ok bootif=01-$macdash sha256=$sum"
				;;
			esac
		else
			note -e "$hlog" 'serial not asserted (the poll stops the VM at the squashfs request); the server log above is the evidence'
		fi
		boots1=$(boots_count "$entry") || boots1=
		check -d "${boots0:-unread} -> ${boots1:-unread}" \
			"hephaestus_utility_boots_total{entry=\"$entry\"} rose" rose "$boots0" "$boots1"
		;;
	esac

	check "boot_next is NULL for $mac" flag_cleared "$mac"
	cleared1=$(audit_count "$mac" "$reason") || cleared1=
	check -d "${cleared0:-unread} -> ${cleared1:-unread}" "one boot_next.cleared audit with reason $reason" one_more "$cleared0" "$cleared1"
}

# --- the install phase (M3, design 5.10) ---
# lab_pending: `cluster list --json` shows a pending pair for lab.
lab_pending() { heph cluster list --json | box jq -e '.[] | select(.name == "lab") | .pending_pair_id != ""'; }
# machine_is <mac> <jq filter>: `machine show --json` of the machine satisfies the filter.
machine_is() { heph machine show "$1" --json | box jq -e "$2"; }
# tool_curl <curl args...>: curl from a container on the subnet, trusting only the test CA, over TLS 1.3, with the
# hephaestus FQDN resolved to hephaestus.
tool_curl() {
	docker run --rm --network "$NET" -v "$root/$certs:/certs:ro" "$TOOLS" \
		curl --cacert /certs/ca.pem --tlsv1.3 --resolve "$FQDN:443:$HEPH_IP" "$@"
}
# served_controlplane: $cfg, the body /config served, is a control-plane machine config.
served_controlplane() { printf '%s\n' "$cfg" | grep -q 'type: controlplane'; }
# ticket_not_in <log>: the log is not empty and does not hold $token, the ticket the install script carried.
ticket_not_in() { [ -n "$token" ] && [ -s "$1" ] && ! printf '%s\n' "$token" | grep -qF -f /dev/stdin "$1"; }
# no_fixture_secret <log>: the log is not empty and holds none of the fixture's secret values.
no_fixture_secret() {
	vals=$(box jq -r '.[]' <"$root/test/e2e/fixture-cluster/values.json") || return 1
	[ -n "$vals" ] && [ -s "$1" ] || return 1
	! printf '%s\n' "$vals" | grep -qF -f /dev/stdin "$1"
}

# machine_approve <mac> <host>: approves the machine into lab/<host>.
machine_approve() {
	check -e "$out/machine-approve-$2.txt" "machine approve $1 lab/$2 exits 0" heph_to "$out/machine-approve-$2.txt" machine approve "$1" "lab/$2"
}

# cluster_setup <mac>: imports the fixture cluster from the git host, approves its first pair once it is pending, and
# approves the machine into lab/cp-01.
cluster_setup() {
	check -e "$out/cluster-import.txt" "cluster import lab exits 0" heph_to "$out/cluster-import.txt" cluster import lab --repo "https://$GIT_FQDN/lab.git" --ref main --path lab
	check -e "$out/cluster-list.json" "cluster list --json shows a pending pair for lab within 120 polls" wait_for 'a pending pair for lab' 120 lab_pending
	heph cluster list --json >"$out/cluster-list.json" 2>&1 || true
	pair=$(box jq -r '.[] | select(.name == "lab") | .pending_pair_id' <"$out/cluster-list.json" || true)
	check -d "${pair:-none}" -e "$out/cluster-approve.txt" "cluster approve lab <pending pair> exits 0" heph_to "$out/cluster-approve.txt" cluster approve lab "$pair"
	machine_approve "$1" cp-01
}

# install_boot <name> <firmware> <mac> <arch> <host>: boots the machine, approved into lab/<host>, until iPXE runs the
# fake kernel the install script loaded (the upstreams program serves an iPXE script as the kernel, which prints
# HEPHAESTUS-E2E-TALOS-KERNEL), then judges each leg: row 10 install, the kernel and initramfs from hephaestus, the ready
# Talos row, the live ticket. A BIOS VM writes to VGA only, so its evidence is the initramfs request in the server log, the
# last file the install script loads before it boots the kernel.
install_boot() {
	name=$1 fw=$2 mac=$3 arch=$4 host=$5
	uuid=e2e00000-0000-4000-8000-$(printf '%s' "$mac" | tr -d :)
	serial=$out/$name.serial.log
	st=$out/$name.serial.txt
	base=/t/$TALOS_SCHEMATIC/$TALOS_VERSION/$arch
	uefi=yes
	[ "$fw" = seabios ] && uefi=no
	if [ "$uefi" = yes ]; then firmware=UEFI; else firmware=BIOS; fi
	step "$name" "Install boot $name" arch="$arch" firmware=$firmware host="$host"
	hm=$(mark heph-e2e-hephaestus)
	: >"$serial"
	: >"$st"
	docker exec heph-e2e-qemu /e2e/run-vm.sh "$name" "$fw" "$mac" "$uuid" "$((INSTALL_TIMEOUT + 10))" >"$out/$name.vm.log" 2>&1 &
	vm=$!

	started=$(date +%s)
	result=timeout
	while [ "$(($(date +%s) - started))" -lt "$INSTALL_TIMEOUT" ]; do
		tr -d '\r' <"$serial" >"$st"
		if [ "$uefi" = yes ]; then
			grep -qF 'HEPHAESTUS-E2E-TALOS-KERNEL booted' "$st" && { result=reached; break; }
			grep -qE 'Could not boot|giving up' "$st" && { result=failed; break; }
		else
			since heph-e2e-hephaestus "$hm" >"$out/$name.poll.log"
			ge "$(jn "$out/$name.poll.log" --arg p "$base/initramfs" 'fromjson? | select(.msg=="request" and .path==$p and .status==200)')" 1 && { result=reached; break; }
		fi
		kill -0 "$vm" 2>/dev/null || { result=exited; break; }
		sleep 2
	done
	elapsed=$(($(date +%s) - started))
	docker exec heph-e2e-qemu pkill -f qemu-system >/dev/null 2>&1 || true
	wait "$vm" || true
	rm -f "$out/$name.poll.log"
	tr -d '\r' <"$serial" >"$st"
	since heph-e2e-hephaestus "$hm" >"$out/$name.hephaestus.log"
	docker logs heph-e2e-dnsmasq >"$out/dnsmasq-$heph_mode.log" 2>&1 || true
	hlog=$out/$name.hephaestus.log

	if [ "$uefi" = yes ]; then
		what='iPXE booted the fake Talos kernel (serial: HEPHAESTUS-E2E-TALOS-KERNEL booted)'
	else
		what='iPXE loaded the initramfs the install script names (the request in the server log)'
	fi
	if [ "$result" = reached ]; then
		pass -d "after ${elapsed}s" -e "$st" -e "$hlog" "$what within ${INSTALL_TIMEOUT}s"
	else
		fail -d "$result after ${elapsed}s" -e "$st" -e "$hlog" "$what within ${INSTALL_TIMEOUT}s"
		tail -n 15 "$st" | sed 's/^/        serial| /'
	fi
	re=$(lease_re "$out/dnsmasq-$heph_mode.log" "$mac")
	check -e "$out/dnsmasq-$heph_mode.log" "dnsmasq leased an address to $mac" test -n "$re"
	[ -n "$re" ] || re='^no-lease$' # an empty pattern would match every address
	check -e "$hlog" "$(basename "$hlog") is not empty" test -s "$hlog"
	n=$(jn "$hlog" --arg ip "$re" 'fromjson? | select(.msg=="request" and .path=="/boot" and .status==200 and .tls==true and .decision_row==10 and .action=="install" and (.remote_ip|test($ip)))')
	check -e "$hlog" "/boot 200 row 10, action install, over TLS" ge "$n" 1
	for f in kernel initramfs; do
		n=$(jn "$hlog" --arg ip "$re" --arg p "$base/$f" 'fromjson? | select(.msg=="request" and .path==$p and .status==200 and .tls==false and (.remote_ip|test($ip)))')
		check -e "$hlog" "GET $base/$f 200 from the VM over plain HTTP" ge "$n" 1
	done
	heph machine show "$mac" --json >"$out/$name.machine.json" 2>&1 || true
	check -e "$out/$name.machine.json" "machine show: assigned to lab/$host" machine_is "$mac" '.state == "assigned" and .cluster == "lab" and .host == "'"$host"'"'
	check -e "$out/$name.machine.json" "machine show: the Talos row is ready" machine_is "$mac" '.talos.state == "ready"'
	check -e "$out/$name.machine.json" "machine show: a live ticket, its config not yet asked for" machine_is "$mac" '.ticket != null and .ticket.config_requested == false'
}

# config_fetch <name> <mac>: Talos's own HTTPS /config fetch cannot trust the test CA (it stays lab test 3), so the
# harness stands in for it. It asks /boot as the machine from a container on the subnet, as iPXE does (the machine's
# UUID, MAC, serial and product, so row 2 sees no other identity), takes the talos.config URL from the install script and
# fetches it twice: the first is served and recorded, the second refused, since a ticket is single-use. The script and the
# config carry the ticket and the fixture's secret values, so neither is written to the output directory.
config_fetch() {
	name=$1 mac=$2
	uuid=e2e00000-0000-4000-8000-$(printf '%s' "$mac" | tr -d :)
	step config-fetch "Config fetch $name (the harness in Talos's place)" machine="$mac"
	hm=$(mark heph-e2e-hephaestus)
	heph machine show "$mac" --json >"$out/$name.before.json" 2>&1 || true
	ser=$(box jq -r '.serial' <"$out/$name.before.json" || true)
	prod=$(box jq -r '.product' <"$out/$name.before.json" || true)
	script=$(tool_curl -fsS -G "https://$FQDN/boot" --data-urlencode "uuid=$uuid" \
		--data-urlencode "mac=$(printf '%s' "$mac" | tr : -)" --data-urlencode arch=x86_64 --data-urlencode platform=efi \
		--data-urlencode "serial=$ser" --data-urlencode "product=$prod" 2>"$out/$name.boot.err" || true)
	url=$(printf '%s\n' "$script" | sed -n 's/.* talos\.config=\([^ ]*\).*/\1/p' | sed 's/\${uuid}/'"$uuid"'/')
	token=${url##*&t=}
	check -e "$out/$name.boot.err" "/boot as the machine answers an install script with a talos.config URL and a ticket" \
		sh -c 'case $1 in "https://$2/config?u=$3&t="?*) ;; *) exit 1 ;; esac' _ "$url" "$FQDN" "$uuid"
	cfg=$(tool_curl -fsS "$url" 2>"$out/$name.config.err" || true)
	check -e "$out/$name.config.err" "GET /config with the ticket is served a control-plane machine config" served_controlplane
	code=$(tool_curl -s -o /dev/null -w '%{http_code}' "$url" || true)
	check -d "${code:-none}" "the same ticket again gets 404" test "$code" = 404
	since heph-e2e-hephaestus "$hm" >"$out/$name.hephaestus.log"
	hlog=$out/$name.hephaestus.log
	n=$(jn "$hlog" 'fromjson? | select(.msg=="request" and .path=="/config" and .status==200 and .outcome=="served")')
	check -d "${n:-0}" -e "$hlog" "hephaestus logged exactly one /config 200 served" test "${n:-0}" -eq 1
	n=$(jn "$hlog" 'fromjson? | select(.msg=="request" and .path=="/config" and .status==404 and (.outcome|startswith("refused_")))')
	check -d "${n:-0}" -e "$hlog" "hephaestus logged exactly one /config 404 refusal" test "${n:-0}" -eq 1
	check -e "$hlog" "the ticket appears nowhere in the hephaestus log" ticket_not_in "$hlog"
	check -e "$out/events.jsonl" "the ticket appears nowhere in events.jsonl" ticket_not_in "$out/events.jsonl"
	check -e "$out/run.log" "the ticket appears nowhere in run.log so far" ticket_not_in "$out/run.log"
	unset script cfg url token
	check "machine show: installing, with no live ticket (the delivery is recorded)" machine_is "$mac" '.state == "installing" and .ticket == null'
	heph audit list --machine "$mac" --json >"$out/$name.audit.json" 2>&1 || true
	n=$(box jq '[.[] | select(.action == "config.served")] | length' <"$out/$name.audit.json" || true)
	check -d "${n:-unread}" -e "$out/$name.audit.json" "one config.served audit event" test "${n:-0}" -eq 1
}

# upstreams_checks <arch>...: the upstreams program's request log shows hephaestus cloning lab.git and following the
# factory's 302 to the assets host for each Talos part of each arch; no fixture secret value appears in either program's log.
upstreams_checks() {
	step upstreams-log "The upstreams program's log, and no fixture secret value in the captured logs"
	ulog=$out/upstreams.log
	docker logs heph-e2e-upstreams >"$ulog" 2>&1 || true
	n=$(jn "$ulog" --arg h "$GIT_FQDN" 'fromjson? | select(.msg=="request" and .host==$h and .status==200 and (.path|startswith("/lab.git/")))')
	check -d "${n:-0}" -e "$ulog" "hephaestus fetched lab.git over smart HTTP" ge "$n" 1
	for arch; do
		for f in "kernel-$arch" "initramfs-$arch.xz" "cmdline-metal-$arch"; do
			n=$(jn "$ulog" --arg h "$FACTORY_FQDN" --arg p "/image/$TALOS_SCHEMATIC/$TALOS_VERSION/$f" 'fromjson? | select(.msg=="request" and .host==$h and .path==$p and .status==302)')
			check -e "$ulog" "$FACTORY_FQDN answered the image request for $f with a 302" ge "$n" 1
		done
	done
	n=$(jn "$ulog" --arg h "$ASSETS_FQDN" 'fromjson? | select(.msg=="request" and .host==$h and (.status==200 or .status==206) and (.path|startswith("/assets/")))')
	check -d "${n:-0}" -e "$ulog" "hephaestus followed the 302s to $ASSETS_FQDN, which served $(($# * 3)) or more assets" ge "$n" "$(($# * 3))"
	docker logs heph-e2e-hephaestus >"$out/install-phase.hephaestus.log" 2>&1 || true
	check -e "$out/install-phase.hephaestus.log" "no fixture secret value appears in the hephaestus log" no_fixture_secret "$out/install-phase.hephaestus.log"
	check -e "$ulog" "no fixture secret value appears in the upstreams log" no_fixture_secret "$ulog"
	check -e "$out/events.jsonl" "no fixture secret value appears in events.jsonl" no_fixture_secret "$out/events.jsonl"
	check -e "$out/run.log" "no fixture secret value appears in run.log so far" no_fixture_secret "$out/run.log"
}

# --- main ---
# The Docker daemon's own architecture, so images run natively. Only colima on arm64 has been tested; `task
# ipxe:build:testca` still builds iPXE in a linux/arm64 builder (taskfile.yml).
case $(docker info --format '{{.Architecture}}') in
aarch64 | arm64) PLATFORM=linux/arm64 ;;
x86_64) PLATFORM=linux/amd64 ;;
*)
	echo "e2e: unsupported Docker architecture: $(docker info --format '{{.Architecture}}')" >&2
	exit 1
	;;
esac

step leftovers 'Clean up anything a killed run left behind'
remove_all
rm -rf "${tree:?}" "${upstream:?}"
mkdir -p "$out" "$certs" build/ipxe/out

step toolbox 'Toolbox image'
docker build --platform "$PLATFORM" -q -t "$TOOLS" test/e2e/qemu >/dev/null

step certs 'Test CA and leaf certificates'
docker run --rm --user "$(id -u):$(id -g)" -v "$root/$certs:/certs" -v "$root/test/e2e:/e2e:ro" "$TOOLS" sh /e2e/gen-certs.sh >"$out/gen-certs.log" 2>&1

step ipxe 'iPXE built to trust only the test CA'
[ -e "$testca_pem" ] || own_pem=yes
[ -e "$testca_dir" ] || own_testca=yes
cp "$certs/ca.pem" "$testca_pem"
"$TASK" ipxe:build:testca TEST_CA="$testca_pem" >"$out/ipxe-build.log" 2>&1 || { tail -n 30 "$out/ipxe-build.log"; exit 1; }
for f in undionly.kpxe snp-amd64.efi snp-arm64.efi roots.pem; do
	[ -s "$testca_dir/$f" ] || { echo "e2e: $testca_dir/$f missing after the testca build" >&2; exit 1; }
done

step network 'Network'
docker network create --driver bridge --subnet 10.99.0.0/24 --gateway "$GATEWAY" "$NET" >/dev/null
docker volume create "$VOL" >/dev/null

step upstream "Fixture image and the TLS 1.3 fake upstream at $UPSTREAM_IP"
docker build --platform "$PLATFORM" -q -t "$FIXTURE" test/e2e/fixture >"$out/fixture-build.log" 2>&1 || { tail -n 30 "$out/fixture-build.log"; exit 1; }
mkdir "$upstream"
docker run --rm --user "$(id -u):$(id -g)" -v "$root/$upstream:/out" "$FIXTURE" cp -r /fixture/. /out/
docker run -d --name heph-e2e-upstream --network "$NET" --ip "$UPSTREAM_IP" \
	-v "$root/test/e2e:/caddy:ro" -v "$root/$certs:/certs:ro" -v "$root/$upstream:/srv:ro" \
	"$CADDY_IMAGE" caddy run --config /caddy/upstream.json >/dev/null
wait_for "the upstream listening" 30 sh -c 'docker logs heph-e2e-upstream 2>&1 | grep -q "serving initial configuration"'

step upstreams "The upstreams program at $UPSTREAMS_IP: fake image factory, its assets host and the fixture cluster's git host"
start_upstreams
upstreams_tls_checks

step tree 'Temporary copy of the tracked tree, with the e2e catalog overlay and the test-CA binaries'
# The repository tree and the production Dockerfile never hold the overlay or test-CA binaries: they live only in this
# copy. sh has no pipefail, so the archive goes through a file and each tar's status is checked (set -e): a tracked file
# that is missing from the working tree fails the run instead of building an image without it. The overlay is copied from
# the working tree, which the archive does not hold until it is committed.
mkdir "$tree"
git ls-files -z >"$out/tree.files"
tar --null -T "$out/tree.files" -cf "$out/tree.tar"
tar -xf "$out/tree.tar" -C "$tree"
rm -f "$out/tree.files" "$out/tree.tar"
cp "$root/test/e2e/overlay/catalog.go.in" "$tree/internal/catalog/catalog.go"

step pin 'Pin the e2e catalog against the fake upstream with the real pin tool'
docker build --platform "$PLATFORM" -q -f "$tree/cmd/catalogpin/Dockerfile" --build-arg VERSION=e2e -t "$PIN_IMAGE" "$tree" >"$out/pin-image-build.log" 2>&1 ||
	{ tail -n 30 "$out/pin-image-build.log"; exit 1; }
mkdir -p "$tree/build/e2e/run" # the pin tool writes its report there, inside the bind-mounted tree
# The pin container reaches the upstream by name, so SNI matches, and trusts only the e2e CA, over TLS 1.3 like hephaestus.
docker run --rm --name heph-e2e-pin --network "$NET" --user "$(id -u):$(id -g)" -w /src \
	--add-host "$UPSTREAM_FQDN:$UPSTREAM_IP" -e SSL_CERT_FILE=/certs/ca.pem \
	-v "$root/$certs:/certs:ro" -v "$root/$tree:/src" "$PIN_IMAGE" \
	-out internal/catalog/catalog_pins.go -keys internal/catalog/keys -provenance internal/catalog/keys/PROVENANCE.md \
	-work /work -report build/e2e/run/pin-report.txt >"$out/pin.out" 2>"$out/pin.err" || { cat "$out/pin.err"; exit 1; }
cp "$tree/build/e2e/run/pin-report.txt" "$out/pin-report.txt"
cp "$tree/internal/catalog/catalog_pins.go" "$out/catalog_pins.go.txt" # not .go: golangci-lint typechecks Go files under build/
check -e "$out/pin.out" -e "$out/pin-report.txt" "the catalogpin summary says PINNED 4 ABSENT 0" grep -qx 'PINNED 4 ABSENT 0' "$out/pin.out"
ev_all_passed || { cat "$out/pin-report.txt"; exit 1; }

step image 'hephaestus:e2e from that copy, with the test-CA binaries, the factory base and the fixture resolver overlaid'
# The test-CA binaries go in only now: the pin image does not need them, and changing its build context every run (the test
# CA is new every run) would rerun its go build. DATE is the HEAD commit's, so a per-run value does not invalidate the
# build cache from the first RUN after ARG DATE on. The two overlays point the factory at the upstreams program and resolve
# references from the fixture's values.json, embedded beside the resolver (//go:embed paths are package-relative).
cp "$testca_dir"/* "$tree/internal/boot/ipxe/bin/"
cp "$root/test/e2e/overlay/base.go.in" "$tree/internal/factory/base.go"
cp "$root/test/e2e/overlay/resolver.go.in" "$tree/internal/secrets/onepassword.go"
cp "$root/test/e2e/fixture-cluster/values.json" "$tree/internal/secrets/e2e_values.json"
docker buildx build --platform "$PLATFORM" --load -q \
	--build-arg VERSION=e2e --build-arg COMMIT="$(git rev-parse --short HEAD)" \
	--build-arg DATE="$(TZ=UTC git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd)" \
	-t "$IMAGE" "$tree" >"$out/image-build.log" 2>&1 || { tail -n 30 "$out/image-build.log"; exit 1; }
rm -rf "${tree:?}"

step start-file 'dnsmasq and hephaestus' mode=file
docker run -d --name heph-e2e-qemu --network "$NET" --ip "$QEMU_IP" --cap-add NET_ADMIN --device /dev/net/tun \
	-v "$root/test/e2e/qemu:/e2e:ro" -v "$root/$out:/out" "$TOOLS" sleep infinity >/dev/null
start_dnsmasq "$HEPH_IP"
start_hephaestus file
check -e "$out/unknown-action.txt" "unknown-action set hold --yes exits 0" heph_to "$out/unknown-action.txt" unknown-action set hold --yes
check "settings: unknown_action is hold" unknown_is_hold

step run-level-file 'Run-level checks' mode=file
tls_checks file "$HEPH_IP" heph-e2e-hephaestus
capability_checks

boot ovmf ovmf 52:54:00:e2:e0:03 amd64 snp.efi snp.efi file
boot seabios seabios 52:54:00:e2:e0:04 amd64 tftp://$HEPH_IP/undionly.kpxe undionly.kpxe file
boot aarch64 aarch64 52:54:00:e2:e0:05 arm64 snp-arm64.efi snp-arm64.efi file

# The three M1 machines are known now, which boot-next set needs. Each case boots one of them once more.
step utility-fetch 'Utility phase (design 5.9): fetch the e2e entries from the fake upstream'
utility_setup

step utility-case-1 'Case 1: EFI chain (e2e-shell, a zip) on amd64 UEFI and arm64 UEFI'
utility_boot shell-ovmf ovmf 52:54:00:e2:e0:03 amd64 e2e-shell shell ShellBinPkg/UefiShell/X64/Shell.efi
utility_boot shell-aarch64 aarch64 52:54:00:e2:e0:05 arm64 e2e-shell shell ShellBinPkg/UefiShell/AArch64/Shell.efi

step utility-case-2 'Case 2: kernel, initrd and squashfs (e2e-live: an ISO on amd64, a zip on arm64) on amd64 UEFI, amd64 BIOS and arm64 UEFI'
utility_boot live-ovmf ovmf 52:54:00:e2:e0:03 amd64 e2e-live live boot/vmlinuz boot/initrd.img live/filesystem.squashfs
utility_boot live-seabios seabios 52:54:00:e2:e0:04 amd64 e2e-live live boot/vmlinuz boot/initrd.img live/filesystem.squashfs
utility_boot live-aarch64 aarch64 52:54:00:e2:e0:05 arm64 e2e-live live live/vmlinuz live/initrd.img live/filesystem.squashfs

step utility-case-3 'Case 3: a UEFI-only entry set on the BIOS VM is cleared as unsupported and the VM holds at row 7'
utility_boot unsupported-seabios seabios 52:54:00:e2:e0:04 amd64 e2e-shell unsupported

step install "Install phase (M3): import the fixture cluster from $GIT_FQDN, approve its pair and one machine per firmware into lab" \
	cluster=lab hosts=cp-01,wrk-01,wrk-02
cluster_setup 52:54:00:e2:e0:03
machine_approve 52:54:00:e2:e0:04 wrk-01
machine_approve 52:54:00:e2:e0:05 wrk-02
install_boot install-ovmf ovmf 52:54:00:e2:e0:03 amd64 cp-01
config_fetch install-ovmf 52:54:00:e2:e0:03
install_boot install-seabios seabios 52:54:00:e2:e0:04 amd64 wrk-01
install_boot install-aarch64 aarch64 52:54:00:e2:e0:05 arm64 wrk-02
upstreams_checks amd64 arm64

step proxy "Proxy mode: Caddy at $CADDY_IP terminates TLS, hephaestus serves plain HTTP behind it" mode=proxy
save_logs
docker rm -f heph-e2e-hephaestus heph-e2e-dnsmasq >/dev/null
heph_mode=proxy
start_dnsmasq "$CADDY_IP"
start_hephaestus proxy
docker run -d --name heph-e2e-caddy --network "$NET" --ip "$CADDY_IP" -v "$root/test/e2e:/caddy:ro" -v "$root/$certs:/certs:ro" \
	"$CADDY_IMAGE" caddy run --config /caddy/caddy.json >/dev/null
wait_for "Caddy listening" 30 sh -c 'docker logs heph-e2e-caddy 2>&1 | grep -q "serving initial configuration"'
tls_checks proxy "$CADDY_IP" heph-e2e-caddy
code=$(box curl -s -o /dev/null -w '%{http_code}' "http://$HEPH_IP:8443/boot" || true)
check -d "${code:-none}" "a direct request from a non-proxy peer gets 403" test "$code" = 403
boot ovmf-proxy ovmf 52:54:00:e2:e0:06 amd64 snp.efi snp.efi proxy

step sqlite 'SQLite dump'
sqlite_q .dump >"$out/sqlite.dump.sql"
check -e "$out/sqlite.dump.sql" "sqlite dump holds the four machines" test "$(grep -c 'INSERT INTO machines' "$out/sqlite.dump.sql")" -eq 4

ev_all_passed
