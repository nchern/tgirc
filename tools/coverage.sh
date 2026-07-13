#!/bin/sh -ue

pkg="./pkg/...,./tgirc/..."

coverprofile="artifacts/coverage.out"

coverprofile_raw="${coverprofile}.raw"

report_text="artifacts/coverage_report_last.txt"

covermode=${COVER_MODE:-"set"}

go test -coverpkg="$pkg" -coverprofile="$coverprofile_raw" -covermode="$covermode" ./...
cat "$coverprofile_raw" \
    | grep -v ".pb." \
    | grep -v ".xo." \
    > $coverprofile

if [ "${1:-""}" = html ]; then
    go tool cover -html="$coverprofile"
else
    go tool cover -func="$coverprofile" | tee "$report_text"
fi
