#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BUILD="$ROOT/build"
APP="$BUILD/Up.app"
DMG_ROOT="$BUILD/dmg"

rm -rf "$APP" "$DMG_ROOT" "$BUILD/Up.dmg"
mkdir -p "$APP/Contents/MacOS" "$DMG_ROOT"

cp "$ROOT/packaging/Info.plist" "$APP/Contents/Info.plist"
go build -trimpath -ldflags="-s -w" -o "$APP/Contents/MacOS/Up" "$ROOT"
codesign --force --deep --sign - "$APP"

cp -R "$APP" "$DMG_ROOT/Up.app"
ln -s /Applications "$DMG_ROOT/Applications"
hdiutil create -quiet -volname Up -srcfolder "$DMG_ROOT" -ov -format UDZO "$BUILD/Up.dmg"
rm -rf "$DMG_ROOT"

printf 'Created %s\nCreated %s\n' "$APP" "$BUILD/Up.dmg"