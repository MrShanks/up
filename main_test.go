package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *app {
	t.Helper()
	return &app{
		dir:           t.TempDir(),
		allowDelete:   false,
		pairingToken:  "pair-secret",
		pairingExpiry: time.Now().Add(time.Minute),
		devices: map[string]pairedDevice{
			"known-device": {ID: "known-device", Token: "device-secret", Name: "Android · Chrome", LastSeen: time.Now()},
		},
		quit: make(chan struct{}),
	}
}

func request(method, target, remote string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = remote
	return req
}

func TestPrivateNetworkOnlyRejectsPublicAddress(t *testing.T) {
	recorder := httptest.NewRecorder()
	privateNetworkOnly(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("public request reached handler")
	})).ServeHTTP(recorder, request(http.MethodGet, "https://up/", "8.8.8.8:1234"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestFileRoutesRequirePairedDevice(t *testing.T) {
	application := testApp(t)
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, request(http.MethodGet, "https://up/device/wrong/api/files", "192.168.1.20:1234"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestPairingIsOneTimeAndRedirectsToDeviceCapability(t *testing.T) {
	application := testApp(t)
	first := httptest.NewRecorder()
	application.routes().ServeHTTP(first, request(http.MethodGet, "https://up/pair/pair-secret", "192.168.1.20:1234"))
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first pairing status = %d, want %d", first.Code, http.StatusSeeOther)
	}
	location := first.Header().Get("Location")
	if !strings.HasPrefix(location, "/device/") || location == "/device/device-secret/" {
		t.Fatalf("unexpected independent device location = %q", location)
	}
	if cookies := first.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("pairing unexpectedly set cookies: %+v", cookies)
	}

	second := httptest.NewRecorder()
	application.routes().ServeHTTP(second, request(http.MethodGet, "https://up/pair/pair-secret", "192.168.1.20:1234"))
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("reused pairing status = %d, want %d", second.Code, http.StatusUnauthorized)
	}
}

func TestTLSClientFollowsPairingRedirectWithCapability(t *testing.T) {
	application := testApp(t)
	server := httptest.NewTLSServer(application.routes())
	defer server.Close()
	client := server.Client()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Test server certificate.
	response, err := client.Get(server.URL + "/pair/pair-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Request.URL.Path, "/device/") {
		t.Fatalf("status = %d, path = %q", response.StatusCode, response.Request.URL.Path)
	}
}

func TestPairedDeviceCanListFiles(t *testing.T) {
	application := testApp(t)
	if err := os.WriteFile(filepath.Join(application.dir, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := request(http.MethodGet, "https://up/device/device-secret/api/files", "192.168.1.20:1234")
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "hello.txt") {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
}

func TestPhonePageUsesCapabilityScopedAPIs(t *testing.T) {
	application := testApp(t)
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, request(http.MethodGet, "https://up/device/device-secret/", "192.168.1.20:1234"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "/device/device-secret/api/files") || strings.Contains(body, "/app/api/files") {
		t.Fatalf("phone page did not contain capability-scoped API path")
	}
	if strings.Contains(body, "#send{") || !strings.Contains(body, "#send-button{") {
		t.Fatalf("phone page has conflicting Send panel styles")
	}
}

func TestDeleteDisabledByDefault(t *testing.T) {
	application := testApp(t)
	file := filepath.Join(application.dir, "keep.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := request(http.MethodDelete, "https://up/device/device-secret/files/keep.txt", "192.168.1.20:1234")
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("file was deleted: %v", err)
	}
}

func TestRevokeInvalidatesExistingDevice(t *testing.T) {
	application := testApp(t)
	revoke := httptest.NewRecorder()
	application.routes().ServeHTTP(revoke, request(http.MethodPost, "https://localhost/revoke", "127.0.0.1:1234"))
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", revoke.Code)
	}

	req := request(http.MethodGet, "https://up/device/device-secret/api/files", "192.168.1.20:1234")
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("old session status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestDashboardListsPairedDevices(t *testing.T) {
	application := testApp(t)
	application.devices["second"] = pairedDevice{ID: "second", Token: "second-secret", Name: "Android", Address: "192.168.1.21", LastSeen: time.Now()}
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, request(http.MethodGet, "https://localhost/", "127.0.0.1:1234"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "Android · Chrome") || !strings.Contains(body, "Android") || !strings.Contains(body, "192.168.1.21") {
		t.Fatalf("dashboard did not list paired devices")
	}
}

func TestDevicesAPIListsPairedDevices(t *testing.T) {
	application := testApp(t)
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, request(http.MethodGet, "https://localhost/api/devices", "127.0.0.1:1234"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"id":"known-device"`) || !strings.Contains(body, `"name":"Android · Chrome"`) {
		t.Fatalf("devices response = %q", body)
	}

	remote := httptest.NewRecorder()
	application.routes().ServeHTTP(remote, request(http.MethodGet, "https://up/api/devices", "192.168.1.20:1234"))
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote status = %d, want %d", remote.Code, http.StatusForbidden)
	}
}

func TestDevicesAPIHidesDisconnectedDevices(t *testing.T) {
	application := testApp(t)
	device := application.devices["known-device"]
	device.LastSeen = time.Now().Add(-deviceOnlineWindow - time.Second)
	application.devices["known-device"] = device

	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, request(http.MethodGet, "https://localhost/api/devices", "127.0.0.1:1234"))
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "known-device") {
		t.Fatalf("disconnected device response = %q", recorder.Body.String())
	}

	reconnect := httptest.NewRecorder()
	application.routes().ServeHTTP(reconnect, request(http.MethodGet, "https://up/device/device-secret/api/files", "192.168.1.20:1234"))
	if reconnect.Code != http.StatusOK {
		t.Fatalf("reconnect status = %d", reconnect.Code)
	}
	if len(application.deviceViews()) != 1 {
		t.Fatal("authorized device did not become connected again")
	}
}

func TestRevokeOneDeviceLeavesOtherAuthorized(t *testing.T) {
	application := testApp(t)
	application.devices["second"] = pairedDevice{ID: "second", Token: "second-secret", Name: "Android", LastSeen: time.Now()}
	revoke := httptest.NewRecorder()
	application.routes().ServeHTTP(revoke, request(http.MethodDelete, "https://localhost/devices/known-device", "127.0.0.1:1234"))
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", revoke.Code)
	}

	removed := httptest.NewRecorder()
	application.routes().ServeHTTP(removed, request(http.MethodGet, "https://up/device/device-secret/api/files", "192.168.1.20:1234"))
	if removed.Code != http.StatusUnauthorized {
		t.Fatalf("removed device status = %d", removed.Code)
	}
	remaining := httptest.NewRecorder()
	application.routes().ServeHTTP(remaining, request(http.MethodGet, "https://up/device/second-secret/api/files", "192.168.1.21:1234"))
	if remaining.Code != http.StatusOK {
		t.Fatalf("remaining device status = %d", remaining.Code)
	}
}

func TestLocalControlRejectsCrossOriginRequest(t *testing.T) {
	application := testApp(t)
	req := request(http.MethodPost, "https://localhost/revoke", "127.0.0.1:1234")
	req.Header.Set("Origin", "https://attacker.example")
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestLocalControlAllowsMatchingHTTPOrigin(t *testing.T) {
	application := testApp(t)
	req := request(http.MethodGet, "http://localhost:54321/api/devices", "127.0.0.1:1234")
	req.Header.Set("Origin", "http://localhost:54321")
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
}

func TestRotatePairingKeepsHTTPSPhoneAddress(t *testing.T) {
	application := testApp(t)
	application.setPhoneURL("https://192.168.1.10:18443/pair/old-token")
	req := request(http.MethodPost, "http://localhost:54321/rotate", "127.0.0.1:1234")
	req.Header.Set("Origin", "http://localhost:54321")
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if phoneURL := application.currentPhoneURL(); !strings.HasPrefix(phoneURL, "https://192.168.1.10:18443/pair/") || strings.HasSuffix(phoneURL, "/old-token") {
		t.Fatalf("phone URL = %q", phoneURL)
	}
}

func TestDownloadRejectsSymlink(t *testing.T) {
	application := testApp(t)
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(application.dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	req := request(http.MethodGet, "https://up/device/device-secret/files/link.txt", "192.168.1.20:1234")
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}
