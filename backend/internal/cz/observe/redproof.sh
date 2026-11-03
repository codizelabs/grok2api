#!/usr/bin/env bash
# P6 red proof: break each guard on purpose and show that its test goes red.
# Every break is applied to a copy of one file, the test runs, and the file is
# restored (also on Ctrl-C). Exit 0 only when every break was caught.
set -u
cd "$(dirname "$0")/../../.." # backend/
fails=0
backup=""
restore() { [ -n "$backup" ] && [ -f "$backup" ] && mv "$backup" "${backup%.redproof}"; backup=""; }
trap restore EXIT INT TERM

# break <file> <perl substitution> <package> <test regex> <what>
brk() {
  local file=$1 sub=$2 pkg=$3 run=$4 what=$5
  backup="$file.redproof"
  cp "$file" "$backup"
  perl -0pi -e "$sub" "$file"
  if cmp -s "$file" "$backup"; then
    echo "SETUP ERROR  $what: the break did not apply"
    fails=$((fails + 1))
  elif ! go test -count=1 "$pkg" -run '^$' >/dev/null 2>&1; then
    echo "SETUP ERROR  $what: the broken code does not compile (a red build is not a caught break)"
    fails=$((fails + 1))
  elif go test -count=1 "$pkg" -run "$run" >/dev/null 2>&1; then
    echo "NOT CAUGHT   $what"
    fails=$((fails + 1))
  else
    echo "caught (red) $what"
  fi
  restore
}

O=internal/cz/observe
brk $O/observe.go 's/return string\(r\[:2\]\) \+ "…" \+ string\(r\[len\(r\)-2:\]\)/return value/' ./$O 'TestNoCredentialOrPromptInAnyResponse' \
  'a broken mask (MaskIdentity returns the e-mail) leaks a credential'
brk $O/observe.go 's/func ErrorCode\(raw string\) string \{/func ErrorCode(raw string) string {\n\tif raw != "" {\n\t\treturn raw\n\t}/' ./$O 'TestNoCredentialOrPromptInAnyResponse' \
  'free-text last error passed through (may quote a token or e-mail)'
brk $O/observe.go 's/label = s.accountLabel\(\*r.AccountID\)/label = r.AccountName/' ./$O 'TestNoCredentialOrPromptInAnyResponse' \
  'the request log shows the account name instead of the label'
brk $O/observe.go 's/out.Proxies = append\(out.Proxies, Proxy\{Label: "proxy_" \+ s.labels.Ref\("egress-node:"\+strconv.FormatUint\(n.ID, 10\)\)/out.Proxies = append(out.Proxies, Proxy{Label: n.ProxyDisplay + strconv.FormatUint(n.ID, 10)/' ./$O 'TestNoCredentialOrPromptInAnyResponse' \
  'a proxy shown by its URL instead of a label'
brk $O/handler.go 's/\tfor _, method := range writeMethods \{\n\t\tgroup.Handle\(method, "\/\*path", methodNotAllowed\)\n\t\}\n//' ./$O 'TestWriteMethodsAre405' \
  'the 405 for write methods removed'
brk $O/handler.go 's/configured := service != nil && len\(token\) >= MinTokenLength/configured := service != nil/' ./$O 'TestUnconfiguredIs503' \
  'an empty or short observe token accepted'
brk $O/handler.go 's/len\(raw\) != len\(token\) \|\| subtle.ConstantTimeCompare\(\[\]byte\(raw\), \[\]byte\(token\)\) != 1/subtle.ConstantTimeCompare([]byte(raw), []byte(raw)) != 1/' ./$O 'TestTokenIsRequiredAndScoped' \
  'any bearer token accepted'
brk internal/app/cz_observe.go 's/if len\(token\) < observe.MinTokenLength \{/if false {/' ./internal/app 'TestCZObserveIsFeatureOnly' \
  'P6 switched on with no observe token'
brk internal/transport/http/server.go 's/\tregisterCZObserve\(router, deps\)\n//' ./internal/transport/http 'TestCZObserveRoutesAreMountedReadOnlyAndUnconfiguredByDefault' \
  'P6 not mounted (the base)'

if [ $fails -eq 0 ]; then echo "P6 red proof: every break caught"; else echo "P6 red proof: $fails break(s) not caught"; fi
exit $fails
