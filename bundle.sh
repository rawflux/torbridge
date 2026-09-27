#!/bin/sh
# Builds the installer packages into bin/:
#   TorBridge-Setup-<version>-windows-x64.zip
#   TorBridge-Setup-<version>-linux-x64.tar.gz
# plus SHA256SUMS. Needs Go and the Tor Expert Bundles for both platforms in dist/.
set -e
cd "$(dirname "$0")"
ver=$(sed -n 's/^const Version = "\(.*\)"/\1/p' internal/bridge/bridge.go)
[ -n "$ver" ] || { echo "cannot read Version" >&2; exit 1; }
rm -rf bin && mkdir -p bin

build() { # goos, suffix, outdir
    GOOS=$1 GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$3/torbridge$2" ./cmd/torbridge
}
bundle() { # platform pattern
    b=$(ls dist/tor-expert-bundle-$1-x86_64-*.tar.gz 2>/dev/null | sort -V | tail -n 1)
    [ -n "$b" ] || { echo "no Tor Expert Bundle for $1 in dist/" >&2; exit 1; }
    echo "$b"
}

w=TorBridge-Setup-$ver-windows-x64
mkdir -p "bin/$w"
build windows .exe "bin/$w"
cp "$(bundle windows)" setup/windows/install.ps1 "bin/$w/"
for f in Install.cmd Uninstall.cmd README.txt; do sed 's/\r*$/\r/' "setup/windows/$f" > "bin/$w/$f"; done
for f in LICENSE LEGAL.md; do sed 's/\r*$/\r/' "$f" > "bin/$w/$f"; done
(cd bin && rm -f "$w.zip" && zip -qr "$w.zip" "$w")

l=TorBridge-Setup-$ver-linux-x64
mkdir -p "bin/$l"
build linux "" "bin/$l"
cp "$(bundle linux)" setup/linux/install.sh setup/linux/README.txt LICENSE LEGAL.md "bin/$l/"
chmod 755 "bin/$l/install.sh" "bin/$l/torbridge"
tar -C bin --owner=0 --group=0 -czf "bin/$l.tar.gz" "$l"

(cd bin && sha256sum "$w.zip" "$l.tar.gz" > SHA256SUMS)
ls -l bin/*.zip bin/*.tar.gz bin/SHA256SUMS
