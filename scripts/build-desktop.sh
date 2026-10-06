#!/usr/bin/env bash
set -euo pipefail
export CGO_ENABLED=1

os=$(go env GOOS)
arch=$(go env GOARCH)
out=${DESKTOP_OUT:-dist/desktop}
desktop_version=${DESKTOP_VERSION:-dev}
mkdir -p "$out"
# Numeric x.y.z for the macOS bundle and Windows version resource.
numeric_version=${desktop_version#v}
numeric_version=${numeric_version%%-*}
if [[ ! $numeric_version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then numeric_version=0.1.0; fi
ldver="-X main.version=$desktop_version -X main.commit=$(git rev-parse --short HEAD 2>/dev/null || echo none) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

case "$os/$arch" in
  darwin/arm64)
    app="$out/PR Manager.app"
    mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
    cp assets/icon/icon.icns "$app/Contents/Resources/icon.icns"
    CGO_LDFLAGS="${CGO_LDFLAGS:-} -framework UniformTypeIdentifiers" \
      go build -tags desktop,production -trimpath -ldflags "-s -w $ldver" -o "$app/Contents/MacOS/pr-manager" .
    cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>PR Manager</string>
<key>CFBundleDisplayName</key><string>PR Manager</string>
<key>CFBundleIdentifier</key><string>com.amitbet.pr-manager</string>
<key>CFBundleExecutable</key><string>pr-manager</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$numeric_version</string>
<key>CFBundleIconFile</key><string>icon</string>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
    # Seal the whole bundle, including Info.plist. Without this, Gatekeeper
    # reports a downloaded copy as damaged instead of offering Open Anyway.
    codesign --force --deep --sign - "$app"
    codesign --verify --deep --strict "$app"
    ditto -c -k --keepParent "$app" "$out/PR-Manager-macos-arm64.zip"
    ;;
  linux/amd64)
    go build -tags desktop,production,webkit2_41 -trimpath -ldflags "-s -w $ldver" -o "$out/pr-manager-linux-amd64" .
    # The command line too, which runs without GTK (a server, a CI job).
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w $ldver" -o "$out/pr-manager" .
    tar -C "$out" -czf "$out/pr-manager-linux-amd64.tar.gz" pr-manager-linux-amd64 pr-manager
    ;;
  windows/amd64)
    # Icon and version metadata. go build links the .syso into the exe.
    trap 'rm -f rsrc_windows_amd64.syso' EXIT
    go run github.com/tc-hib/go-winres@v0.3.3 make --in assets/icon/winres.json --arch amd64 --out rsrc \
      --product-version "$numeric_version" --file-version "$numeric_version"
    go build -tags desktop,production -trimpath -ldflags "-s -w $ldver -H windowsgui" -o "$out/pr-manager-windows-amd64.exe" .
    ;;
  *)
    echo "unsupported desktop target: $os/$arch" >&2
    exit 1
    ;;
esac
