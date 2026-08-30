package main

import (
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
		sessionToken:  "device-secret",
		quit:          make(chan struct{}),
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
	application.routes().ServeHTTP(recorder, request(http.MethodGet, "https://up/app/api/files", "192.168.1.20:1234"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestPairingIsOneTimeAndSetsSecureCookie(t *testing.T) {
	application := testApp(t)
	first := httptest.NewRecorder()
	application.routes().ServeHTTP(first, request(http.MethodGet, "https://up/pair/pair-secret", "192.168.1.20:1234"))
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first pairing status = %d, want %d", first.Code, http.StatusSeeOther)
	}
	cookie := first.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie security flags missing: %+v", cookie)
	}

	second := httptest.NewRecorder()
	application.routes().ServeHTTP(second, request(http.MethodGet, "https://up/pair/pair-secret", "192.168.1.20:1234"))
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("reused pairing status = %d, want %d", second.Code, http.StatusUnauthorized)
	}
}

func TestPairedDeviceCanListFiles(t *testing.T) {
	application := testApp(t)
	if err := os.WriteFile(filepath.Join(application.dir, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := request(http.MethodGet, "https://up/app/api/files", "192.168.1.20:1234")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: application.sessionToken})
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "hello.txt") {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
}

func TestDeleteDisabledByDefault(t *testing.T) {
	application := testApp(t)
	file := filepath.Join(application.dir, "keep.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := request(http.MethodDelete, "https://up/app/files/keep.txt", "192.168.1.20:1234")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: application.sessionToken})
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

	req := request(http.MethodGet, "https://up/app/api/files", "192.168.1.20:1234")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "device-secret"})
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("old session status = %d, want %d", recorder.Code, http.StatusUnauthorized)
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

func TestDownloadRejectsSymlink(t *testing.T) {
	application := testApp(t)
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(application.dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	req := request(http.MethodGet, "https://up/app/files/link.txt", "192.168.1.20:1234")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: application.sessionToken})
	recorder := httptest.NewRecorder()
	application.routes().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}
