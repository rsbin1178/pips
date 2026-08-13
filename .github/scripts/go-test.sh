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
		/"Action":"output"/ && /"Test":"/ {
			line = $0
			package_field = ""
			test_field = ""
			if (match(line, /"Package":"[^"]+"/)) {
				package_field = substr(line, RSTART, RLENGTH)
			}
			if (match(line, /"Test":"[^"]+"/)) {
				test_field = substr(line, RSTART, RLENGTH)
			}
			if (package_field != "" && test_field != "") {
				key = package_field SUBSEP test_field
				if (details[key] == "") {
					details[key] = line
				} else {
					details[key] = details[key] " | " line
				}
				if (length(details[key]) > 12000) {
					details[key] = substr(details[key], length(details[key]) - 11999)
				}
			}
		}
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
				key = "\"Package\":\"" package_name "\"" SUBSEP "\"Test\":\"" test_name "\""
				if (test_name != "" && details[key] != "") {
					failure = failure " | " details[key]
				}
				print "::error title=Go test failed::" failure
			}
		}
	' "$report"
fi

exit "$status"
