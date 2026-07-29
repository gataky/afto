#!/bin/sh
# afto-echo.sh — the smallest possible afto plugin, in POSIX shell.
#
# It suggests "<buffer>--help" for any non-empty buffer. Useless in
# practice; the point is that this is the ENTIRE contract: read a JSON
# request per line from stdin, write a JSON response per line to stdout,
# echo the id back. No SDK, no linking, no language requirement.
#
# Try it:
#   [[plugin]]
#   name    = "echo"
#   command = "/path/to/afto-echo.sh"
#
# Extracting fields with sed is fine for a demo. A real plugin should use a
# JSON parser — a command line can contain anything, including the quotes
# and braces this kind of pattern matching assumes it will not see.

while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  buffer=$(printf '%s' "$line" | sed -n 's/.*"buffer":"\([^"]*\)".*/\1/p')
  [ -z "$id" ] && continue

  if [ -n "$buffer" ]; then
    printf '{"v":1,"id":%s,"candidates":[{"text":"%s--help","note":"from afto-echo.sh"}]}\n' \
      "$id" "$buffer"
  else
    printf '{"v":1,"id":%s,"candidates":[]}\n' "$id"
  fi
done
