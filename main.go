package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	maxUploadSize   int64 = 10 << 30
	pairingLifetime       = 10 * time.Minute
)

type app struct {
	dir           string
	phoneURL      string
	qrCode        []byte
	allowDelete   bool
	devicesFile   string
	pairingToken  string
	pairingExpiry time.Time
	devices       map[string]pairedDevice
	quit          chan struct{}
	quitOnce      sync.Once
	mu            sync.RWMutex
}

type fileInfo struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type pairedDevice struct {
	ID       string    `json:"id"`
	Token    string    `json:"token"`
	Name     string    `json:"name"`
	Address  string    `json:"address"`
	PairedAt time.Time `json:"pairedAt"`
	LastSeen time.Time `json:"lastSeen"`
}

type deviceView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	LastSeen string `json:"lastSeen"`
}

type dashboardData struct {
	PhoneURL    string
	AllowDelete bool
	Devices     []deviceView
}

func loadDevices(path string) (map[string]pairedDevice, error) {
	devices := make(map[string]pairedDevice)
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return devices, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(contents, &devices); err != nil {
		return nil, err
	}
	return devices, nil
}

func (a *app) saveDevicesLocked() error {
	if a.devicesFile == "" {
		return nil
	}
	contents, err := json.MarshalIndent(a.devices, "", "  ")
	if err != nil {
		return err
	}
	temporary := a.devicesFile + ".tmp"
	if err := os.WriteFile(temporary, contents, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, a.devicesFile)
}

func (a *app) currentPhoneURL() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.phoneURL
}

func (a *app) deviceViews() []deviceView {
	a.mu.RLock()
	defer a.mu.RUnlock()
	views := make([]deviceView, 0, len(a.devices))
	for _, device := range a.devices {
		views = append(views, deviceView{ID: device.ID, Name: device.Name, Address: device.Address, LastSeen: relativeTime(device.LastSeen)})
	}
	sort.Slice(views, func(i, j int) bool { return a.devices[views[i].ID].LastSeen.After(a.devices[views[j].ID].LastSeen) })
	return views
}

func (a *app) addDevice(r *http.Request) (pairedDevice, error) {
	id, err := newToken()
	if err != nil {
		return pairedDevice{}, err
	}
	token, err := newToken()
	if err != nil {
		return pairedDevice{}, err
	}
	now := time.Now()
	device := pairedDevice{ID: id, Token: token, Name: deviceName(r.UserAgent()), Address: requestIP(r).String(), PairedAt: now, LastSeen: now}
	a.mu.Lock()
	a.devices[id] = device
	err = a.saveDevicesLocked()
	a.mu.Unlock()
	return device, err
}

func (a *app) authorizeDevice(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, device := range a.devices {
		if secureEqual(token, device.Token) {
			device.LastSeen = time.Now()
			a.devices[id] = device
			return true
		}
	}
	return false
}

func deviceName(userAgent string) string {
	lower := strings.ToLower(userAgent)
	switch {
	case strings.Contains(lower, "android") && strings.Contains(lower, "chrome"):
		return "Android · Chrome"
	case strings.Contains(lower, "android"):
		return "Android"
	case strings.Contains(lower, "iphone") || strings.Contains(lower, "ipad"):
		return "iPhone / iPad"
	case strings.Contains(lower, "firefox"):
		return "Firefox"
	case strings.Contains(lower, "chrome"):
		return "Chrome"
	default:
		return "Mobile device"
	}
}

func relativeTime(value time.Time) string {
	age := time.Since(value)
	switch {
	case age < time.Minute:
		return "now"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return value.Format("Jan 2")
	}
}

func main() {
	defaultDir, err := defaultTransferDir()
	if err != nil {
		log.Fatal(err)
	}
	defaultConfig, err := defaultConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	dir := flag.String("dir", defaultDir, "folder used for uploads and downloads")
	configDir := flag.String("config", defaultConfig, "folder used for certificate and device credentials")
	port := flag.Int("port", 8080, "TCP port (0 chooses an available port)")
	allowDelete := flag.Bool("allow-delete", false, "allow paired phones to delete shared files")
	flag.Parse()

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		log.Fatalf("create transfer folder: %v", err)
	}
	if err := os.MkdirAll(*configDir, 0o700); err != nil {
		log.Fatalf("create config folder: %v", err)
	}
	certFile, keyFile, err := ensureCertificate(*configDir)
	if err != nil {
		log.Fatalf("prepare HTTPS certificate: %v", err)
	}
	devicesFile := filepath.Join(*configDir, "devices.json")
	devices, err := loadDevices(devicesFile)
	if err != nil {
		log.Fatalf("load paired devices: %v", err)
	}
	pairingToken, err := newToken()
	if err != nil {
		log.Fatalf("create pairing token: %v", err)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatal(err)
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	application := &app{
		dir:           absDir,
		allowDelete:   *allowDelete,
		devicesFile:   devicesFile,
		pairingToken:  pairingToken,
		pairingExpiry: time.Now().Add(pairingLifetime),
		devices:       devices,
		quit:          make(chan struct{}),
	}
	application.setPhoneURL(fmt.Sprintf("https://%s:%d/pair/%s", localIP(), actualPort, pairingToken))
	server := &http.Server{
		Handler:           application.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	dashboardURL := fmt.Sprintf("https://localhost:%d/", actualPort)
	log.Printf("Sharing %s", absDir)
	log.Printf("Pair a phone at %s", dashboardURL)
	log.Print("HTTPS uses a private local certificate; accept the warning only for this Up address.")
	if err := openBrowser(dashboardURL); err != nil {
		log.Printf("Could not open browser: %v", err)
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.ServeTLS(listener, certFile, keyFile) }()
	select {
	case <-application.quit:
		_ = server.Close()
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /qr.png", a.qrImage)
	mux.HandleFunc("GET /api/devices", a.localControl(a.listDevices))
	mux.HandleFunc("POST /open-folder", a.localControl(a.openFolder))
	mux.HandleFunc("POST /rotate", a.localControl(a.rotatePairing))
	mux.HandleFunc("POST /revoke", a.localControl(a.revokeDevices))
	mux.HandleFunc("DELETE /devices/{id}", a.localControl(a.revokeDevice))
	mux.HandleFunc("POST /quit", a.localControl(a.quitApp))
	mux.HandleFunc("GET /pair/{token}", a.pairDevice)
	mux.HandleFunc("GET /device/{session}/{$}", a.requireDevice(a.index))
	mux.HandleFunc("GET /device/{session}/api/files", a.requireDevice(a.listFiles))
	mux.HandleFunc("POST /device/{session}/api/upload", a.requireDevice(a.uploadFiles))
	mux.HandleFunc("GET /device/{session}/files/{name}", a.requireDevice(a.downloadFile))
	mux.HandleFunc("DELETE /device/{session}/files/{name}", a.requireDevice(a.deleteFile))
	return securityHeaders(privateNetworkOnly(mux))
}

func (a *app) setPhoneURL(url string) {
	qrCode, err := qrcode.Encode(url, qrcode.Medium, 384)
	if err != nil {
		log.Printf("create QR code: %v", err)
		return
	}
	a.mu.Lock()
	a.phoneURL = url
	a.qrCode = qrCode
	a.mu.Unlock()
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	data := dashboardData{PhoneURL: a.currentPhoneURL(), AllowDelete: a.allowDelete, Devices: a.deviceViews()}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := futuristicDashboardTemplate.Execute(w, data); err != nil {
		log.Printf("render dashboard: %v", err)
	}
}

func (a *app) qrImage(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	a.mu.RLock()
	image := append([]byte(nil), a.qrCode...)
	a.mu.RUnlock()
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(image)
}

func (a *app) listDevices(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.deviceViews())
}

func (a *app) pairDevice(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	valid := time.Now().Before(a.pairingExpiry) && secureEqual(r.PathValue("token"), a.pairingToken)
	if valid {
		a.pairingToken = ""
		a.pairingExpiry = time.Time{}
	}
	a.mu.Unlock()
	if !valid {
		http.Error(w, "This pairing code is invalid or has expired. Create a new one on the computer.", http.StatusUnauthorized)
		return
	}
	device, err := a.addDevice(r)
	if err != nil {
		http.Error(w, "Could not save paired device", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/device/"+device.Token+"/", http.StatusSeeOther)
}

func (a *app) requireDevice(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authorizeDevice(r.PathValue("session")) {
			http.Error(w, "Pair this device using the QR code on the computer.", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *app) localControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requestIsLocal(r) || !sameOrigin(r) {
			http.Error(w, "Local dashboard access required", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (a *app) rotatePairing(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	token, err := newToken()
	if err != nil {
		http.Error(w, "Could not create pairing code", http.StatusInternalServerError)
		return
	}
	host := localIP()
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "Could not determine server port", http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	a.pairingToken = token
	a.pairingExpiry = time.Now().Add(pairingLifetime)
	a.mu.Unlock()
	a.setPhoneURL(fmt.Sprintf("https://%s:%s/pair/%s", host, port, token))
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) revokeDevices(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	a.mu.Lock()
	a.devices = make(map[string]pairedDevice)
	err := a.saveDevicesLocked()
	a.mu.Unlock()
	if err != nil {
		http.Error(w, "Could not revoke devices", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) revokeDevice(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	delete(a.devices, r.PathValue("id"))
	err := a.saveDevicesLocked()
	a.mu.Unlock()
	if err != nil {
		http.Error(w, "Could not revoke device", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) openFolder(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	if err := openPath(a.dir); err != nil {
		http.Error(w, "Could not open shared folder", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) quitApp(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	a.quitOnce.Do(func() { close(a.quit) })
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct{ AllowDelete bool }{a.allowDelete}
	var page bytes.Buffer
	if err := futuristicPhoneTemplate.Execute(&page, data); err != nil {
		log.Printf("render phone page: %v", err)
		return
	}
	base := "/device/" + r.PathValue("session") + "/"
	_, _ = io.WriteString(w, strings.ReplaceAll(page.String(), "/app/", base))
}

func (a *app) listFiles(w http.ResponseWriter, _ *http.Request) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		http.Error(w, "Could not read transfer folder", http.StatusInternalServerError)
		return
	}
	files := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			files = append(files, fileInfo{Name: entry.Name(), Size: info.Size(), Modified: info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Modified.After(files[j].Modified) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(files)
}

func (a *app) uploadFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Expected a multipart upload", http.StatusBadRequest)
		return
	}
	uploaded := make([]fileInfo, 0)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			http.Error(w, "Upload interrupted", http.StatusBadRequest)
			return
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}
		info, err := a.savePart(part)
		_ = part.Close()
		if err != nil {
			http.Error(w, "Could not save file", http.StatusInternalServerError)
			return
		}
		uploaded = append(uploaded, info)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(uploaded)
}

func (a *app) savePart(part *multipart.Part) (fileInfo, error) {
	name := safeName(part.FileName())
	if name == "" {
		name = "upload"
	}
	path, name, err := availablePath(a.dir, name)
	if err != nil {
		return fileInfo{}, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fileInfo{}, err
	}
	size, copyErr := io.Copy(file, part)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return fileInfo{}, errors.Join(copyErr, closeErr)
	}
	return fileInfo{Name: name, Size: size, Modified: time.Now()}, nil
}

func (a *app) downloadFile(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.PathValue("name"))
	if name == "" || name != r.PathValue("name") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(a.dir, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

func (a *app) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !a.allowDelete {
		http.Error(w, "Deleting from a phone is disabled", http.StatusForbidden)
		return
	}
	name := safeName(r.PathValue("name"))
	if name == "" || name != r.PathValue("name") {
		http.NotFound(w, r)
		return
	}
	if err := os.Remove(filepath.Join(a.dir, name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Could not delete file", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func safeName(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "." || name == "" {
		return ""
	}
	return name
}

func availablePath(dir, name string) (string, string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for number := 0; number < 10_000; number++ {
		candidate := name
		if number > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, number, ext)
		}
		path := filepath.Join(dir, candidate)
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return path, candidate, nil
		}
		if err != nil {
			return "", "", err
		}
	}
	return "", "", errors.New("could not choose a unique filename")
}

func newToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func ensureCertificate(configDir string) (string, string, error) {
	certFile := filepath.Join(configDir, "certificate.pem")
	keyFile := filepath.Join(configDir, "certificate-key.pem")
	if certificateCovers(certFile, localIP()) {
		if _, err := os.Stat(keyFile); err == nil {
			return certFile, keyFile, nil
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return "", "", err
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Up Local Transfer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1"), net.ParseIP(localIP())},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

func certificateCovers(path, host string) bool {
	contents, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(contents)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || time.Now().After(certificate.NotAfter) {
		return false
	}
	return certificate.VerifyHostname(host) == nil && certificate.VerifyHostname("localhost") == nil
}

func defaultTransferDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Downloads", "Up"), nil
}

func defaultConfigDir() (string, error) {
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "Up"), nil
}

func localIP() string {
	connection, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer connection.Close()
	return connection.LocalAddr().(*net.UDPAddr).IP.String()
}

func requestIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func requestIsLocal(r *http.Request) bool {
	ip := requestIP(r)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "https" && parsed.Host == r.Host
}

func privateNetworkOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := requestIP(r)
		if ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) {
			http.Error(w, "Up is available only on a private local network.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func secureEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func openBrowser(url string) error { return openPath(url) }

func openPath(path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", path)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		command = exec.Command("xdg-open", path)
	}
	return command.Start()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Up - Pair phone</title>
<style>:root{--ink:#17211d;--paper:#f4f1e8;--green:#0f715b;--red:#b53c29;--line:#c9c7ba}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;color:var(--ink);background:repeating-linear-gradient(0deg,transparent 0 31px,rgba(23,33,29,.04) 31px 32px),var(--paper);font-family:Georgia,'Times New Roman',serif}main{width:min(520px,calc(100% - 32px));padding:40px 0;text-align:center}h1{margin:0 0 10px;font-size:64px;font-weight:500;letter-spacing:0}p{margin:0 0 20px;font-size:19px;line-height:1.4}.qr{display:block;width:min(384px,100%);margin:auto;border:1px solid var(--line)}.url,.note{margin-top:16px;overflow-wrap:anywhere;color:var(--green);font:12px/1.5 ui-monospace,monospace}.note{color:#62675f}.actions{display:grid;grid-template-columns:1fr 1fr;gap:10px;margin-top:24px}.actions button{min-height:48px;border:0;border-radius:3px;background:var(--green);color:white;font:700 13px ui-monospace,monospace;cursor:pointer}.actions .quit{background:transparent;border:1px solid var(--line);color:var(--red)}</style></head>
<body><main><h1>Up</h1><p>Scan once to pair this phone securely.</p><img class="qr" src="/qr.png" alt="One-time QR code for pairing"><div class="url">{{.PhoneURL}}</div><div class="note">Pairing expires in 10 minutes and works once. Phone deletion: {{if .AllowDelete}}enabled{{else}}disabled{{end}}.</div><div class="actions"><button id="folder">Open shared folder</button><button id="rotate">New pairing code</button><button id="revoke">Forget paired phones</button><button class="quit" id="quit">Quit Up</button></div></main><script>document.querySelector('#folder').onclick=()=>fetch('/open-folder',{method:'POST'});document.querySelector('#rotate').onclick=async()=>{await fetch('/rotate',{method:'POST'});location.reload()};document.querySelector('#revoke').onclick=async()=>{if(confirm('Disconnect every paired phone?')){await fetch('/revoke',{method:'POST'});alert('Paired phones disconnected. Create a new pairing code to reconnect.')}};document.querySelector('#quit').onclick=async()=>{await fetch('/quit',{method:'POST'});document.body.innerHTML='<main><h1>Up</h1><p>Up has stopped. You can close this tab.</p></main>'}</script></body></html>`))

var phoneTemplate = template.Must(template.New("phone").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="theme-color" content="#f4f1e8"><title>Up - Local file transfer</title>
<style>:root{--ink:#17211d;--paper:#f4f1e8;--accent:#e85234;--green:#0f715b;--line:#c9c7ba}*{box-sizing:border-box}body{margin:0;min-height:100svh;color:var(--ink);background:repeating-linear-gradient(0deg,transparent 0 31px,rgba(23,33,29,.04) 31px 32px),var(--paper);font-family:Georgia,'Times New Roman',serif}main{width:min(720px,calc(100% - 32px));margin:auto;padding:40px 0 60px}header{display:flex;align-items:end;justify-content:space-between;padding-bottom:14px;border-bottom:2px solid var(--ink)}h1{margin:0;font-size:64px;line-height:.8;font-weight:500;letter-spacing:0}h2{margin:0 0 8px;font-size:30px;font-weight:500;letter-spacing:0}p{margin:0 0 20px;line-height:1.45}.status{color:var(--green);font:700 12px ui-monospace,monospace;text-transform:uppercase}section{padding:30px 0;border-bottom:1px solid var(--line)}input[type=file]{position:absolute;width:1px;height:1px;opacity:0}.picker{min-height:150px;display:grid;place-items:center;padding:24px;border:2px dashed #777b70;background:rgba(255,255,255,.45);text-align:center;cursor:pointer}.picker strong{display:block;margin-bottom:7px;font-size:20px;font-weight:500}.detail,.meta,.empty,#notice{font:12px/1.45 ui-monospace,monospace;color:#62675f}button{min-height:48px;border:0;border-radius:3px;padding:0 18px;color:white;background:var(--accent);font:700 14px ui-monospace,monospace;cursor:pointer}#send{width:100%;margin-top:12px}#notice{min-height:20px;margin-top:10px;color:var(--green)}.files{margin:18px 0 0;padding:0;list-style:none}.file{display:grid;grid-template-columns:minmax(0,1fr) auto {{if .AllowDelete}}auto{{end}};gap:10px;align-items:center;padding:13px 0;border-top:1px solid var(--line)}.name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:16px}.icon{display:grid;width:42px;min-height:42px;place-items:center;background:var(--green);text-decoration:none;color:white;border-radius:3px;font:bold 20px ui-monospace,monospace}.delete{width:42px;padding:0;background:transparent;border:1px solid var(--line);color:var(--accent);font-size:20px}</style></head>
<body><main><header><h1>Up</h1><div class="status">Paired</div></header><section><h2>Send files</h2><p>Choose photos, videos, or documents from this device.</p><form id="upload"><label class="picker"><input id="files" type="file" name="files" multiple required><span><strong id="selection">Choose files</strong><span class="detail" id="detail">Tap to browse this device</span></span></label><button id="send" type="submit">Send to computer</button></form><div id="notice" role="status"></div></section><section><h2>Shared files</h2><p>Download files from the computer.</p><div id="listing" class="empty">Loading files...</div></section></main>
<script>const input=document.querySelector('#files'),form=document.querySelector('#upload'),send=document.querySelector('#send'),notice=document.querySelector('#notice'),listing=document.querySelector('#listing'),allowDelete={{.AllowDelete}};const size=b=>b<1024?b+' B':b<1048576?(b/1024).toFixed(1)+' KB':(b/1048576).toFixed(1)+' MB';const esc=v=>v.replace(/[&<>']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;'}[c]));input.onchange=()=>{const n=input.files.length;document.querySelector('#selection').textContent=n?n+(n===1?' file selected':' files selected'):'Choose files'};async function load(){const response=await fetch('/app/api/files');if(!response.ok)throw Error('Session expired. Pair again.');const files=await response.json();listing.className=files.length?'':'empty';listing.innerHTML=files.length?'<ul class="files">'+files.map(file=>{const name=esc(file.name),path=encodeURIComponent(file.name);return '<li class="file"><div><div class="name">'+name+'</div><div class="meta">'+size(file.size)+'</div></div><a class="icon" href="/app/files/'+path+'" download title="Download">&#8595;</a>'+(allowDelete?'<button class="delete" data-name="'+name+'" title="Delete">&#215;</button>':'')+'</li>'}).join('')+'</ul>':'No files shared yet.'}form.onsubmit=async event=>{event.preventDefault();send.disabled=true;try{const response=await fetch('/app/api/upload',{method:'POST',body:new FormData(form)});if(!response.ok)throw Error(await response.text());form.reset();notice.textContent='Transfer complete.';await load()}catch(error){notice.textContent=error.message}finally{send.disabled=false}};listing.onclick=async event=>{const button=event.target.closest('.delete');if(!button||!confirm('Delete this file?'))return;await fetch('/app/files/'+encodeURIComponent(button.dataset.name),{method:'DELETE'});load()};load().catch(error=>listing.textContent=error.message);setInterval(()=>load().catch(()=>{}),3000)</script></body></html>`))
