# Up

Up transfers files between a macOS or Windows computer and an Android phone over the local network. The phone uses its browser, so no Android app or cloud account is required.

## Run

Install Go 1.24 or newer, then run:

```sh
go run .
```

The app opens a local dashboard with a QR code. Scan it with an Android phone connected to the same Wi-Fi network. If the browser does not open automatically, use the dashboard URL printed in the terminal. Files are shared through `~/Downloads/Up` by default.

## Send from computer to phone

1. On the Mac dashboard, click **Open shared folder**.
2. Copy the files you want to send into that folder.
3. On the phone, open **Shared files** and tap the download arrow.

Refresh the phone page after adding new files. Files uploaded from the phone also appear in this folder.

Use another folder or port when needed:

```sh
go run . -dir "/path/to/folder" -port 8080
```

The browser interface can upload files to the computer, download shared files, and delete files. Duplicate uploads are renamed instead of overwritten. Each launch creates a new private URL, and uploads are limited to 10 GB per request.

## Build

```sh
go build -o up .
```

For Windows, build on Windows or cross-compile from macOS:

```sh
GOOS=windows GOARCH=amd64 go build -o up.exe .
```

## Install on macOS

An installable disk image is available at `build/Up.dmg`. Open it and drag **Up** into **Applications**.

To rebuild the app and disk image:

```sh
./package-macos.sh
```

The local package is ad-hoc signed. If macOS blocks the first launch, Control-click **Up** in Applications, choose **Open**, then confirm. Public distribution without this prompt requires an Apple Developer ID certificate and notarization.

Traffic is not encrypted. Use a trusted network and stop the command-line app with `Ctrl+C`, or use **Quit Up** on the dashboard.