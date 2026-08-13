#!/usr/bin/env bash

set -euo pipefail

report=$(mktemp "${TMPDIR:-/tmp}/pips-go-test.XXXXXX")
trap 'rm -f "$report"' EXIT

set +e
go test -json "$@" | tee "$report"
status=${PIPESTATUS[0]}
set -e

if ((status != 0)); then
	awk '
		/"Action":"fail"/ {
			package_name = ""
			test_name = ""
			if (match($0, /"Package":"[^"]+"/)) {
				package_name = substr($0, RSTART + 11, RLENGTH - 12)
			}
			if (match($0, /"Test":"[^"]+"/)) {
				test_name = substr($0, RSTART + 8, RLENGTH - 9)
			}
			if (package_name != "") {
				failure = package_name
				if (test_name != "") {
					failure = failure " " test_name
				}
				print "::error title=Go test failed::" failure
			}
		}
	' "$report"
fi

exit "$status"
