#!/bin/sh
# scripts/release-archives-check.sh <dir>: the release's archives in dir. Every CLI archive (canyon_*.tar.gz,
# canyon_*.zip, as make cli-release writes them), the laptop demo's bundle (canyon-deploy.tar.gz) and the chart
# (canyon-chart.tgz) carries Canyon's LICENSE, NOTICE and THIRD_PARTY_NOTICES at its top. Every one of them, and
# Opengrep's source (opengrep-*-source.tar.gz), is clean, as scripts/tarball.sh packs one (#2409): owned by uid
# and gid 0 with no names, with no extended attributes, no AppleDouble ._ entry and no home directory in a path.
# Otherwise this fails naming each archive and what is wrong with it. make install-test runs it on the archive it
# builds, make deploy-bundle-check on the bundle, scripts/publish-release.sh on a release's before it publishes
# anything, and scripts/release-smoke.sh on a published one.
set -eu
dir=${1:?usage: scripts/release-archives-check.sh <dir>}
missing= n=0
for a in "$dir"/canyon_*.tar.gz "$dir"/canyon_*.zip "$dir"/canyon-deploy.tar.gz "$dir"/canyon-chart.tgz; do
  [ -e "$a" ] || continue
  n=$((n + 1))
  case $a in
    */canyon_*) name=$(basename "$a"); name=${name%.tar.gz}; name=${name%.zip} ;;
    *) name=canyon ;; # the bundle and the chart unpack into canyon/
  esac
  case $a in
    *.zip) files=$(python3 -c 'import sys, zipfile; print("\n".join(zipfile.ZipFile(sys.argv[1]).namelist()))' "$a")
           licence=$(python3 -c 'import sys, zipfile; sys.stdout.write(zipfile.ZipFile(sys.argv[1]).read(sys.argv[2]).decode())' "$a" "$name/LICENSE" 2>/dev/null || true) ;;
    *) files=$(tar -tzf "$a")
       licence=$(tar -xzOf "$a" "$name/LICENSE" 2>/dev/null || true) ;;
  esac
  for f in LICENSE NOTICE THIRD_PARTY_NOTICES; do
    printf '%s\n' "$files" | grep -qx "$name/$f" || missing="$missing
  $(basename "$a") has no $name/$f"
  done
  case $licence in # the split's header, then the AGPL-3.0 text, then the Apache-2.0 text (cli/, contracts/)
    *'Portions of this software are licensed as follows:'*'GNU AFFERO GENERAL PUBLIC LICENSE'*'Version 3, 19 November 2007'*'Apache License'*'Version 2.0, January 2004'*) ;;
    *) missing="$missing
  $(basename "$a")'s LICENSE is not Canyon's: the AGPL-3.0 core with cli/ and contracts/ under Apache-2.0" ;;
  esac
  case $a in */canyon-deploy.tar.gz) # the deploy repo's reference set-up, under its own 0BSD licence (NOTICE)
    for f in LICENSE admission/services.yaml argocd/applicationsets.yaml argocd/namespaces.yaml namespaces/Chart.yaml external-secrets/clustersecretstore.yaml; do
      printf '%s\n' "$files" | grep -qx "canyon/deploy/gitops/$f" || missing="$missing
  canyon-deploy.tar.gz has no canyon/deploy/gitops/$f"
    done
    case $(tar -xzOf "$a" canyon/deploy/gitops/LICENSE 2>/dev/null || true) in
      'Copyright (c) 2026 PlatCo GmbH'*'Permission to use, copy, modify, and/or distribute this software for any'*) ;;
      *) missing="$missing
  canyon-deploy.tar.gz's deploy/gitops/LICENSE is not the 0BSD text" ;;
    esac ;;
  esac
done
for a in "$dir"/opengrep-*-source.tar.gz; do
  [ -e "$a" ] && n=$((n + 1))
done
[ "$n" -gt 0 ] || { echo "release-archives-check: no release archive in $dir" >&2; exit 1; }
unclean=$(python3 -I - "$dir" <<'EOF'
import fnmatch, pathlib, sys, tarfile, zipfile

archives = ("canyon_*.tar.gz", "canyon_*.zip", "canyon-deploy.tar.gz", "canyon-chart.tgz", "opengrep-*-source.tar.gz")
for path in sorted(pathlib.Path(sys.argv[1]).iterdir()):
    if not any(fnmatch.fnmatch(path.name, a) for a in archives):
        continue
    if path.suffix == ".zip":
        entries = [(n, None) for n in zipfile.ZipFile(path).namelist()]
    else:
        with tarfile.open(path) as t:
            entries = [(m.name, m) for m in t]
            if t.pax_headers:
                print(f"  {path.name} has global pax headers: {', '.join(sorted(t.pax_headers))}")
    for name, m in entries:
        wrong = []
        parts = pathlib.PurePosixPath(name).parts
        if any(p.startswith("._") or p == "__MACOSX" for p in parts):
            wrong.append("macOS AppleDouble metadata")
        if name.startswith("/") or "/Users/" in "/" + name or "/home/" in "/" + name:
            wrong.append("a home directory in its path")
        if m is not None:
            if m.uname or m.gname:
                wrong.append(f"owner names {m.uname!r}:{m.gname!r}")
            if m.uid or m.gid:
                wrong.append(f"uid:gid {m.uid}:{m.gid}")
            attrs = sorted(k for k in m.pax_headers if "xattr" in k or "com.apple" in k)
            if attrs:
                wrong.append(f"extended attributes {', '.join(attrs)}")
        if wrong:
            print(f"  {path.name}: {name}: {'; '.join(wrong)}")
EOF
)
[ -z "$missing$unclean" ] || { printf 'release-archives-check: %s archives in %s, and%s\n%s\n' "$n" "$dir" "$missing" "$unclean" >&2; exit 1; }
echo "release-archives-check: all $n archives in $dir are clean, and every CLI archive, the bundle and the chart carry LICENSE (AGPL-3.0 and Apache-2.0), NOTICE and THIRD_PARTY_NOTICES, and the bundle deploy/gitops under 0BSD"
