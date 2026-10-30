#!/usr/bin/env bash
# Runs the ghapp-mcp-server demo end to end on this machine, offline.
#
# It builds ghapp-mcp-server and the demo's programs
# into a temp dir, makes a demo App key with openssl, starts fakegithub (a
# stand-in for GitHub) and ghapp on free loopback ports, and runs the demo
# client, which talks to fakestraza (a stand-in for Straza). The transcript
# goes to stdout and to $DEMO_OUT, by default a new temp file. Everything
# else, keys and repos included, is removed on exit; DEMO_KEEP=1 keeps it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/.." && pwd)"

for tool in go git openssl; do
	command -v "$tool" >/dev/null || { echo "run.sh: $tool is needed" >&2; exit 1; }
done

work="$(mktemp -d "${TMPDIR:-/tmp}/ghapp-demo.XXXXXX")"
DEMO_OUT="${DEMO_OUT:-$(mktemp "${TMPDIR:-/tmp}/ghapp-demo-transcript.XXXXXX")}"
pids=()

cleanup() {
	local code=$?
	# ${pids[@]+...}: bash 3.2, macOS's, calls an empty array unset.
	for pid in ${pids[@]+"${pids[@]}"}; do
		kill "$pid" 2>/dev/null || true
	done
	for pid in ${pids[@]+"${pids[@]}"}; do
		wait "$pid" 2>/dev/null || true
	done
	if [[ "${DEMO_KEEP:-}" == 1 ]]; then
		echo "run.sh: kept $work" >&2
	else
		rm -rf "$work"
	fi
	exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# wait_port waits until something listens on 127.0.0.1:$1, or $2 has exited.
wait_port() {
	local port=$1 pid=$2 name=$3 log=$4
	for _ in $(seq 1 100); do
		if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
			return 0
		fi
		if ! kill -0 "$pid" 2>/dev/null; then
			echo "run.sh: $name stopped. Its log:" >&2
			cat "$log" >&2
			return 1
		fi
		sleep 0.1
	done
	echo "run.sh: $name did not listen on port $port. Its log:" >&2
	cat "$log" >&2
	return 1
}

bin="$work/bin"
mkdir -p "$bin" "$work/agent" "$work/home"
echo "run.sh: building into $bin" >&2
(
	cd "$root"
	export GOPROXY=off GOFLAGS=-mod=readonly
	go build -o "$bin/ghapp-mcp-server" .
	go build -o "$bin/fakegithub" ./demo/fakegithub
	go build -o "$bin/fakestraza" ./demo/fakestraza
	go build -o "$bin/fakeholder" ./demo/fakeholder
	go build -o "$bin/client" ./demo/client
)

# The demo App's key, as GitHub makes one: PKCS#1 PEM, or PKCS#8 from an
# openssl without -traditional, such as macOS's LibreSSL. ghapp reads both.
# Only fakegithub gets the public half.
openssl genrsa -traditional -out "$work/app-key.pem" 2048 2>/dev/null ||
	openssl genrsa -out "$work/app-key.pem" 2048 2>/dev/null
openssl rsa -in "$work/app-key.pem" -pubout -out "$work/app-pub.pem" 2>/dev/null
gateway_secret="$(openssl rand -hex 32)"
listing_token="$(openssl rand -hex 32)"
app_id=424242

read -r gh_port ghapp_port grants_port holder_port < <("$bin/fakegithub" -free-ports 4)
github="http://127.0.0.1:$gh_port"
ghapp="http://127.0.0.1:$ghapp_port"
grants="http://127.0.0.1:$grants_port"
holder="http://127.0.0.1:$holder_port"

echo "run.sh: starting fakegithub at $github" >&2
env -i PATH="$PATH" HOME="$work/home" \
	"$bin/fakegithub" -listen "127.0.0.1:$gh_port" -app-id "$app_id" \
	-app-public-key "$work/app-pub.pem" -git-root "$work/github" \
	-audit "$work/github-audit.jsonl" 2>"$work/fakegithub.log" &
gh_pid=$!
pids+=("$gh_pid")
wait_port "$gh_port" "$gh_pid" fakegithub "$work/fakegithub.log"

echo "run.sh: starting ghapp-mcp-server at $ghapp" >&2
env -i PATH="$PATH" HOME="$work/home" \
	GHAPP_APP_ID="$app_id" \
	GHAPP_PRIVATE_KEY_FILE="$work/app-key.pem" \
	GHAPP_API_URL="$github" \
	GHAPP_GIT_UPSTREAM="$github" \
	GHAPP_PUBLIC_URL="$ghapp" \
	GHAPP_MCP_GATEWAY_SECRET="$gateway_secret" \
	GHAPP_MCP_LISTING_TOKEN="$listing_token" \
	GHAPP_GIT_GRANT_ISSUER="$grants" \
	GHAPP_MCP_ATTRIBUTES=team \
	"$bin/ghapp-mcp-server" --http "127.0.0.1:$ghapp_port" 2>"$work/ghapp.log" &
ghapp_pid=$!
pids+=("$ghapp_pid")
wait_port "$ghapp_port" "$ghapp_pid" ghapp-mcp-server "$work/ghapp.log"

# Straza's grants and the holder: the agent's plain git goes through the
# holder, which brings ghapp a grant for each request.
echo "run.sh: starting fakestraza grants at $grants and fakeholder at $holder" >&2
env -i PATH="$PATH" HOME="$work/home" \
	FAKESTRAZA_GHAPP_URL="$ghapp/mcp" \
	FAKESTRAZA_GATEWAY_SECRET="$gateway_secret" \
	FAKESTRAZA_LISTING_TOKEN="$listing_token" \
	FAKESTRAZA_ROLES="$here/roles.json" \
	FAKESTRAZA_AGENT=joe-macos-agent \
	FAKESTRAZA_AUDIT="$work/straza-audit.jsonl" \
	FAKESTRAZA_GRANTS_LISTEN="127.0.0.1:$grants_port" \
	FAKESTRAZA_GIT_AUDIENCE="$ghapp" \
	"$bin/fakestraza" grants ghapp 2>"$work/fakestraza-grants.log" &
grants_pid=$!
pids+=("$grants_pid")
wait_port "$grants_port" "$grants_pid" "fakestraza grants" "$work/fakestraza-grants.log"
env -i PATH="$PATH" HOME="$work/home" \
	"$bin/fakeholder" -listen "127.0.0.1:$holder_port" -straza "$grants" 2>"$work/holder.log" &
holder_pid=$!
pids+=("$holder_pid")
wait_port "$holder_port" "$holder_pid" fakeholder "$work/holder.log"

echo "run.sh: running the demo client" >&2
status=0
env -i PATH="$PATH" HOME="$work/home" TMPDIR="$work" LANG=C.UTF-8 \
	FAKESTRAZA_GHAPP_URL="$ghapp/mcp" \
	FAKESTRAZA_GATEWAY_SECRET="$gateway_secret" \
	FAKESTRAZA_LISTING_TOKEN="$listing_token" \
	FAKESTRAZA_ROLES="$here/roles.json" \
	FAKESTRAZA_AGENT=joe-macos-agent \
	FAKESTRAZA_AUDIT="$work/straza-audit.jsonl" \
	"$bin/client" \
	-fakestraza "$bin/fakestraza" \
	-ghapp-url "$ghapp" \
	-ghapp-log "$work/ghapp.log" \
	-github-audit "$work/github-audit.jsonl" \
	-straza-audit "$work/straza-audit.jsonl" \
	-github-git-root "$work/github" \
	-work "$work/agent" \
	-holder-url "$holder" \
	-holder-log "$work/holder.log" \
	-out "$DEMO_OUT" || status=$?

echo "run.sh: transcript in $DEMO_OUT" >&2
if [[ $status -ne 0 ]]; then
	echo "run.sh: the demo failed. ghapp's log:" >&2
	tail -n 40 "$work/ghapp.log" >&2
fi
exit "$status"
