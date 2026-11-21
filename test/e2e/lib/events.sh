# shellcheck shell=sh
# The end-to-end harness's event emitter and output capture, sourced by test/e2e/run.sh. Each call below writes one
# event to <out>/events.jsonl (JSON Lines, test/e2e/events.schema.json) and prints the same event on the console, so
# the record and the console cannot disagree:
#
#   ev_init <out> <run_id> <git_commit> <mode> <colour>  run_start and the run header; <colour> is 1 or 0
#   step <phase> <title> [<key>=<value>]...              ends the open phase (phase_end and its subtotal) and starts
#                                                        the next (phase_start and its header); <phase> is a stable id
#   pass|fail|note [-d <detail>] [-e <file>]... <name>   one check; -e names a captured file, under <out> or relative
#                                                        to it, and may repeat
#   check [-d <detail>] [-e <file>]... <name> <cmd...>   pass when the command succeeds, else fail
#   ev_end <exit_code>                                   ends the open phase, then run_end and the summary
#
# They return 0, so a caller under set -e carries on; check returns 0 whatever its command returned. ev_all_passed
# succeeds while no check has failed. POSIX sh has no local variables, so every variable here starts with ev_ and the
# caller's own are never touched.
#
# capture_start <log> sends this shell's stdout and stderr, merged, to the console and to <log>; capture_finish ends
# that and leaves <log> without colour codes.

EV_SCHEMA=hephaestus.e2e.event/v1
EV_WIDTH=72 # the columns of the header and summary rules
ev_nl='
'
ev_tab=$(printf '\t')

# ev_now sets ev_epoch and ev_ts from one clock reading.
ev_now() {
	read -r ev_epoch ev_ts <<EV_NOW
$(date -u '+%s %Y-%m-%dT%H:%M:%SZ')
EV_NOW
}

# ev_str prints $1 as a JSON string. awk works on bytes under LC_ALL=C, so UTF-8 passes through unchanged. Control
# characters become a backslash, u and four hex digits, the backslash built with %c so that this file holds none.
ev_str() {
	EV_S=$1 LC_ALL=C awk 'BEGIN {
		for (i = 1; i < 32; i++) esc[sprintf("%c", i)] = sprintf("%cu%04x", 92, i)
		esc["\t"] = "\\t"; esc["\n"] = "\\n"; esc["\r"] = "\\r"; esc["\""] = "\\\""; esc["\\"] = "\\\\"
		s = ENVIRON["EV_S"]
		n = length(s)
		out = ""
		for (i = 1; i <= n; i++) {
			c = substr(s, i, 1)
			out = out ((c in esc) ? esc[c] : c)
		}
		printf "\"%s\"", out
	}'
}

# ev_dur <seconds> prints a duration as 14s, 5m12s or 1h02m.
ev_dur() {
	if [ "$1" -lt 60 ]; then
		printf '%ds' "$1"
	elif [ "$1" -lt 3600 ]; then
		printf '%dm%02ds' $(($1 / 60)) $(($1 % 60))
	else
		printf '%dh%02dm' $(($1 / 3600)) $(($1 % 3600 / 60))
	fi
}

# ev_counts <passed> <failed> <notes> <seconds> prints "n passed · n failed · [n notes · ]<duration>".
ev_counts() {
	printf '%s passed · %s failed · ' "$1" "$2"
	if [ "$3" -eq 1 ]; then
		printf '1 note · '
	elif [ "$3" -gt 1 ]; then
		printf '%s notes · ' "$3"
	fi
	ev_dur "$4"
}

# ev_rule <text> <width>: a "━━ <text> ━━…" line EV_WIDTH columns wide. <width> is the text's width, which the caller
# counts from an ASCII twin of the text, since ${#var} counts bytes in some shells.
ev_rule() {
	printf '%s━━ %s %s%s\n' "$ev_hd" "$1" "$(LC_ALL=C awk -v n=$((EV_WIDTH - 4 - $2)) 'BEGIN {
		if (n < 2) n = 2
		for (i = 0; i < n; i++) printf "━"
	}')" "$ev_rst"
}

# ev_emit <type> <members>: appends one event; <members> are its own fields as JSON, each led by a comma.
ev_emit() {
	ev_seq=$((ev_seq + 1))
	printf '{"schema":"%s","ts":"%s","seq":%s,"type":"%s"%s}\n' "$EV_SCHEMA" "$ev_ts" "$ev_seq" "$1" "$2" >>"$ev_file"
}

ev_init() {
	ev_out=$1 ev_file=$1/events.jsonl ev_seq=0 ev_phase=
	ev_passed=0 ev_failed=0 ev_notes=0 ev_rows='' ev_fail_json='' ev_fail_text=''
	ev_hd='' ev_ok='' ev_no='' ev_nt='' ev_dim='' ev_rst=''
	if [ "$5" = 1 ]; then
		ev_esc=$(printf '\033')
		ev_hd="${ev_esc}[1;34m" ev_ok="${ev_esc}[1;32m" ev_no="${ev_esc}[1;31m" ev_nt="${ev_esc}[1;33m"
		ev_dim="${ev_esc}[2m" ev_rst="${ev_esc}[0m"
	fi
	ev_now
	ev_t0=$ev_epoch
	: >"$ev_file"
	ev_emit run_start ",\"run_id\":$(ev_str "$2"),\"out_dir\":$(ev_str "$1"),\"git_commit\":$(ev_str "$3"),\"mode\":$(ev_str "$4")"
	ev_twin="hephaestus e2e - run $2 - $4 mode"
	ev_rule "hephaestus e2e · run $2 · $4 mode" ${#ev_twin}
}

# ev_close ends the open phase, if there is one: phase_end, and, when the phase had checks, the subtotal line and
# the phase's summary row (a setup phase with no checks shows only its header).
ev_close() {
	if [ -z "$ev_phase" ]; then
		return 0
	fi
	ev_now
	ev_d=$((ev_epoch - ev_phase_t0))
	ev_emit phase_end ",\"phase\":$(ev_str "$ev_phase"),\"passed\":$ev_pp,\"failed\":$ev_pf,\"notes\":$ev_pn,\"duration_s\":$ev_d"
	if [ $((ev_pp + ev_pf + ev_pn)) -gt 0 ]; then
		printf '  %s── %s%s\n' "$ev_dim" "$(ev_counts "$ev_pp" "$ev_pf" "$ev_pn" "$ev_d")" "$ev_rst"
		ev_rows="$ev_rows$ev_phase$ev_tab$ev_pp$ev_tab$ev_pf$ev_tab$(ev_dur "$ev_d")$ev_nl"
	fi
	ev_phase=
}

# step <phase> <title> [<key>=<value>]...: the console shows each label's value, "<value> mode" for mode and "cluster
# <value>" for cluster.
step() {
	ev_close
	ev_now
	ev_phase=$1 ev_title=$2 ev_phase_t0=$ev_epoch ev_pp=0 ev_pf=0 ev_pn=0
	shift 2
	ev_lj='' ev_lt=''
	for ev_kv in "$@"; do
		ev_k=${ev_kv%%=*} ev_v=${ev_kv#*=}
		ev_lj="$ev_lj${ev_lj:+,}$(ev_str "$ev_k"):$(ev_str "$ev_v")"
		case $ev_k in
		mode) ev_v="$ev_v mode" ;;
		cluster) ev_v="cluster $ev_v" ;;
		esac
		ev_lt="$ev_lt${ev_lt:+ · }$ev_v"
	done
	ev_emit phase_start ",\"phase\":$(ev_str "$ev_phase"),\"title\":$(ev_str "$ev_title"),\"labels\":{$ev_lj}"
	printf '\n%s▸ %s%s' "$ev_hd" "$ev_title" "$ev_rst"
	if [ -n "$ev_lt" ]; then
		printf '  %s%s%s' "$ev_dim" "$ev_lt" "$ev_rst"
	fi
	printf '\n'
}

# ev_opts reads the leading -d <detail> and -e <file> options into ev_detail, ev_has_detail and ev_files (one per
# line), and sets ev_nopts to how many arguments they took.
ev_opts() {
	ev_detail='' ev_has_detail='' ev_files='' ev_nopts=0
	while [ $# -ge 2 ]; do
		case $1 in
		-d) ev_has_detail=1 ev_detail=$2 ;;
		-e) ev_files="$ev_files$2$ev_nl" ;;
		*) break ;;
		esac
		shift 2
		ev_nopts=$((ev_nopts + 2))
	done
}

pass() {
	ev_opts "$@"
	shift "$ev_nopts"
	ev_check pass "$1"
}
fail() {
	ev_opts "$@"
	shift "$ev_nopts"
	ev_check fail "$1"
}
note() {
	ev_opts "$@"
	shift "$ev_nopts"
	ev_check note "$1"
}
check() {
	ev_opts "$@"
	shift "$ev_nopts"
	ev_name=$1
	shift
	if "$@"; then ev_check pass "$ev_name"; else ev_check fail "$ev_name"; fi
}

# ev_all_passed succeeds when no check has failed so far.
ev_all_passed() { [ "$ev_failed" -eq 0 ]; }

# ev_check <pass|fail|note> <name>, with the options ev_opts read: the check event and its console line. A pass or
# note shows its detail in parentheses; a failure shows it as got:, then its evidence, and is listed again in the
# summary.
ev_check() {
	ev_now
	case $1 in
	pass) ev_pp=$((ev_pp + 1)) ev_passed=$((ev_passed + 1)) ev_w="${ev_ok}PASS$ev_rst" ;;
	fail) ev_pf=$((ev_pf + 1)) ev_failed=$((ev_failed + 1)) ev_w="${ev_no}FAIL$ev_rst" ;;
	note) ev_pn=$((ev_pn + 1)) ev_notes=$((ev_notes + 1)) ev_w="${ev_nt}NOTE$ev_rst" ;;
	esac
	ev_ej='' ev_es=''
	while IFS= read -r ev_f; do
		if [ -z "$ev_f" ]; then
			continue
		fi
		ev_f=${ev_f#"$ev_out"/}
		ev_ej="$ev_ej${ev_ej:+,}$(ev_str "$ev_f")"
		ev_es="$ev_es${ev_es:+, }$ev_f"
	done <<EV_FILES
$ev_files
EV_FILES
	ev_dj=
	if [ -n "$ev_has_detail" ]; then
		ev_dj=",\"detail\":$(ev_str "$ev_detail")"
	fi
	ev_emit check ",\"phase\":$(ev_str "$ev_phase"),\"name\":$(ev_str "$2"),\"result\":\"$1\"$ev_dj,\"evidence\":[$ev_ej],\"elapsed_s\":$((ev_epoch - ev_t0))"

	printf '  %s  %s' "$ev_w" "$2"
	if [ "$1" != fail ] && [ -n "$ev_has_detail" ]; then
		printf ' %s(%s)%s' "$ev_dim" "$ev_detail" "$ev_rst"
	fi
	printf '\n'
	if [ "$1" = fail ]; then
		if [ -n "$ev_has_detail" ]; then
			printf '        %sgot:%s      %s\n' "$ev_dim" "$ev_rst" "$ev_detail"
		fi
		if [ -n "$ev_es" ]; then
			printf '        %sevidence:%s %s\n' "$ev_dim" "$ev_rst" "$ev_es"
		fi
		ev_fail_json="$ev_fail_json${ev_fail_json:+,}{\"phase\":$(ev_str "$ev_phase"),\"name\":$(ev_str "$2")}"
		ev_fail_text="$ev_fail_text  ${ev_no}FAIL$ev_rst  $ev_phase · $2$ev_nl"
	fi
}

# ev_end <exit_code>: run_end, then the summary: a row per phase, the result with the totals, every failure again, and
# where the run's files are.
ev_end() {
	ev_close
	ev_now
	ev_d=$((ev_epoch - ev_t0))
	if [ "$1" -eq 0 ]; then
		ev_r=pass ev_w="${ev_ok}RESULT  PASS$ev_rst"
	else
		ev_r=fail ev_w="${ev_no}RESULT  FAIL$ev_rst"
	fi
	ev_emit run_end ",\"result\":\"$ev_r\",\"passed\":$ev_passed,\"failed\":$ev_failed,\"notes\":$ev_notes,\"duration_s\":$ev_d,\"failures\":[$ev_fail_json],\"exit_code\":$1"

	printf '\n'
	ev_rule Summary 7
	printf '%s' "$ev_rows" | awk -F "$ev_tab" '
		{ id[NR] = $1; p[NR] = $2; f[NR] = $3; t[NR] = $4; if (length($1) > w) w = length($1) }
		END {
			if (NR == 0) exit # no phase had checks, so there is no table
			if (w < 5) w = 5
			fmt = "  %-" w "s  %4s  %4s  %6s\n"
			printf fmt, "phase", "pass", "fail", "time"
			for (i = 1; i <= NR; i++) printf fmt, id[i], p[i], f[i], t[i]
		}'
	printf '\n  %s   %s\n' "$ev_w" "$(ev_counts "$ev_passed" "$ev_failed" "$ev_notes" "$ev_d")"
	printf '%s' "$ev_fail_text"
	if [ "$1" -ne 0 ] && [ "$ev_failed" -eq 0 ]; then
		printf '  stopped: exit status %s with no failed check; run.log holds the cause\n' "$1"
	fi
	printf '\n  record:       %s\n  full output:  %s\n  manifest:     %s\n' \
		"$ev_out/events.jsonl" "$ev_out/run.log" "$ev_out/manifest.json"
}

# capture_start <log>: from here on this shell's stdout and stderr go through a FIFO to tee, which copies them to the
# console and to <log>. The shell stays the process that was started, so every signal sent to it reaches its own
# traps as it would without the capture (a terminal's Ctrl-C to the process group, or kill <pid>); a pipeline or a
# wrapper process would hold a trapped signal until the run ended. tee runs in the background and ignores SIGINT and
# SIGTERM, so it outlives them and copies the cleanup's output too. fds 8 and 9 keep the original stdout and stderr.
capture_start() {
	ev_log=$1
	exec 8>&1 9>&2
	rm -f "$ev_log.fifo"
	mkfifo "$ev_log.fifo"
	(
		trap '' INT TERM
		exec tee "$ev_log" <"$ev_log.fifo"
	) &
	ev_tee=$!
	exec >"$ev_log.fifo" 2>&1
	rm -f "$ev_log.fifo"
}

# capture_finish puts stdout and stderr back, waits for tee to copy the rest (it ends when the FIFO's last writer
# closes it), then rewrites <log> without colour codes. tee does not buffer; <log> holds the codes until here.
capture_finish() {
	exec 1>&8 2>&9 8>&- 9>&-
	wait "$ev_tee" || :
	ev_sgr="$(printf '\033')\[[0-9;]*m"
	LC_ALL=C sed "s/$ev_sgr//g" "$ev_log" >"$ev_log.plain"
	mv "$ev_log.plain" "$ev_log"
}
