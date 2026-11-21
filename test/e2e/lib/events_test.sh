#!/bin/sh
# The emitter's test sample: sh events_test.sh <dir>. TestEvents_* in test/e2e/upstreams/main_test.go run it and
# validate what it writes against test/e2e/events.schema.json. Under a scripted clock it drives events.sh through three
# runs, each writing its events.jsonl to its own directory under <dir>:
#   fail     every event type and check result, a failure with got and evidence, notes, a phase with no checks, every
#            duration form, and names and details holding quotes, backslashes, tabs, newlines, control characters and
#            non-ASCII
#   pass     a run whose every check passed
#   stopped  a run that exited non-zero with no failed check
# Their consoles go to stdout, coloured when E2E_COLOR is 1, as run.sh renders a terminal.
set -eu
. "$(dirname "$0")/events.sh"
colour=${E2E_COLOR:-0}
commit=0123456789abcdef0123456789abcdef01234567

# The scripted clock: date prints the epoch and the RFC 3339 time <clock> seconds after 2026-11-20T00:00:00Z.
clock=0
date() {
	printf '%d 2026-11-20T%02d:%02d:%02dZ\n' $((1795132800 + clock)) $((clock / 3600)) $((clock % 3600 / 60)) $((clock % 60))
}
tick() { clock=$((clock + $1)); }

bs=$(printf '\134')
tab=$(printf '\t')
soh=$(printf '\001')
cr=$(printf '\r')
nl='
'

(
	out=$1/fail
	mkdir -p "$out"
	ev_init "$out" 20261120T000000Z "$commit" file+proxy "$colour"
	step toolbox 'Toolbox image'
	tick 14
	step boot-ovmf 'Boot ovmf' arch=amd64 firmware=UEFI mode=file
	tick 18
	pass -d 'after 18s' -e "$out/ovmf.serial.log" 'held within 120s'
	tick 3
	check -d 10.99.0.131 -e "$out/dnsmasq-file.log" 'dnsmasq leased an address to 52:54:00:e2:e0:03' true
	pass '/start 200 over plain HTTP (tls:false)'
	step boot-seabios 'Boot seabios' arch=amd64 firmware=BIOS mode=file
	tick 11
	pass -d 'after 11s' 'held within 120s'
	note -e "$out/seabios.hephaestus.log" 'serial not asserted (SeaBIOS writes to VGA only); the server log above is the evidence'
	tick 3
	step install-boot 'Install boot install-ovmf' machine=52:54:00:e2:e0:03 cluster=lab host=cp-01
	pass '/boot 200 row 10, action install, over TLS'
	tick 300
	check -d 200 -e "$out/config-fetch-2.txt" -e hephaestus-file.log 'the same ticket again gets 404' false
	tick 12
	note 'a note with neither detail nor evidence'
	pass 'machine show: installing'
	step escapes 'Names and details that need escaping'
	pass -d "detail with a tab${tab}and \"quotes\"" "name with \"quotes\" and a ${bs}backslash"
	fail -d "first line${nl}second line" "name with a newline${nl}and an escape-looking ${bs}u00e9 ${bs}n"
	note -d "control ${soh} and carriage return ${cr} characters" 'non-ASCII: café · ▸ ━ ─ ✓'
	step soak 'A phase over an hour long'
	tick 3720
	pass 'still running'
	ev_end 1
)

(
	out=$1/pass
	mkdir -p "$out"
	clock=0
	ev_init "$out" 20261120T000000Z "$commit" file+proxy "$colour"
	step only 'The only phase' mode=file
	pass 'a check that passed'
	tick 5
	note 'a note'
	ev_end 0
)

(
	out=$1/stopped
	mkdir -p "$out"
	clock=0
	ev_init "$out" 20261120T000000Z "$commit" file+proxy "$colour"
	step ipxe 'iPXE built to trust only the test CA'
	tick 2
	ev_end 1
)
