# Up

Up transfers files between a macOS or Windows computer and an Android phone over the local network. The phone uses its browser, so no Android app or cloud account is required.

## Run

Install Go 1.24 or newer, then run:

```sh
go run .
```

The app opens a local dashboard with a one-time QR pairing code. Scan it with an Android phone connected to the same trusted Wi-Fi network. The code expires after 10 minutes and cannot be reused. Files are shared through `~/Downloads/Up` by default.

The computer dashboard runs over HTTP on localhost, where traffic never leaves the computer and no TLS warning is needed. Phone transfers use a private HTTPS certificate created on first launch. The phone browser will warn that it is not issued by a public certificate authority; verify that the address is your computer's private LAN address before accepting it. Pairing redirects the phone to a persistent, revocable device link that remains valid across app restarts. Treat that link like a password and do not share it.

## Send from computer to phone

1. On the Mac dashboard, click **Open shared folder**.
2. Copy the files you want to send into that folder.
3. On the phone, open **Files** and tap the download arrow.

Refresh the phone page after adding new files. Files uploaded from the phone also appear in this folder.

Use another folder or port when needed:

```sh
go run . -dir "/path/to/folder" -port 8080
```

The browser interface can upload files to the computer and download shared files. Duplicate uploads are renamed instead of overwritten, and uploads are limited to 10 GB per request. Phone-side deletion is disabled by default; enable it explicitly with `-allow-delete`.

The dashboard lists currently connected devices. A device disappears shortly after its page closes and reappears when reopened. Its pairing remains valid until you remove it with **×** or use **Remove all**.

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

## Security

- Phone transfers use HTTPS with a locally generated certificate and private key stored in the user's configuration folder. The dashboard uses a separate HTTP listener bound only to `127.0.0.1`.
- File routes require a long random device capability in the URL; QR pairing codes expire after 10 minutes and work once.
- Connections from public IP addresses are rejected. Do not configure router port forwarding for Up.
- Shared-folder symbolic links are ignored, uploaded files are private to the local user, and phone deletion is off by default.
- The dashboard is loopback-only and can revoke paired devices.

Keep using a trusted local network: encryption protects transfer contents, but accepting a private certificate without verifying its LAN address could still trust the wrong server. Stop the command-line app with `Ctrl+C`, or use **Quit Up** on the dashboard.