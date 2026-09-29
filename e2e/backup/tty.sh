#!/bin/sh
# A real terminal for a command that asks (sourced, not run): script(1)'s
# pty, the answers typed as each prompt appears. Shared by the setup
# suite and the package smoke test, which runs the README's setup line
# as an administrator. Callers set OUT first.

# at_tty <cmd...>: the command at a real terminal, typing what is on
# stdin — for a command that asks nothing. Give it </dev/null: a prompt
# still shows in the output, and script(1) waits 2 s at stdin's end for
# the child to read anything typed that it never asked for [measured].
at_tty() { script -qec "$*" /dev/null; }

# converse <cmd> [<prompt> <answer>]...: the command at a real terminal,
# each answer typed once its prompt is the last thing on the screen —
# a pty echoes what arrives before echo is off, and a loaded runner
# takes its time to a prompt — and then once the screen has moved on,
# so that the same prompt asked again is waited for again. Output in
# $OUT; exit status the command's.
converse() {
	cmd=$1
	shift
	rm -f /root/in
	mkfifo /root/in
	script -qec "$cmd" /dev/null </root/in >"$OUT" 2>&1 &
	cv=$!
	exec 3>/root/in
	while [ $# -ge 2 ]; do
		p=$1
		a=$2
		shift 2
		i=0
		until [ "$(tail -c "${#p}" "$OUT" 2>/dev/null)" = "$p" ] || ! kill -0 "$cv" 2>/dev/null || [ "$i" -ge 600 ]; do
			i=$((i + 1))
			sleep 0.1
		done
		kill -0 "$cv" 2>/dev/null || break
		printf '%s\n' "$a" >&3
		i=0
		while [ "$(tail -c "${#p}" "$OUT" 2>/dev/null)" = "$p" ] && kill -0 "$cv" 2>/dev/null && [ "$i" -lt 100 ]; do
			i=$((i + 1))
			sleep 0.1
		done
	done
	exec 3>&-
	wait "$cv"
}
P_KEY="Storage key id (AWS_ACCESS_KEY_ID): "
P_SECRET="Storage secret key (AWS_SECRET_ACCESS_KEY): "
P_STORED="Type stored to go on: "
P_PW="Repository password: "
