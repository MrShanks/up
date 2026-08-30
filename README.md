# Up

Up transfers files between a macOS or Windows computer and an Android phone over the local network. The phone uses its browser, so no Android app or cloud account is required.

## Run

Install Go 1.24 or newer, then run:

```sh
go run .
```

The app opens a local dashboard with a QR code. Scan it with an Android phone connected to the same Wi-Fi network. If the browser does not open automatically, use the dashboard URL printed in the terminal. Files are shared through `~/Downloads/Up` by default.

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

Traffic is not encrypted. Use a trusted network and stop the app with `Ctrl+C` when finished.