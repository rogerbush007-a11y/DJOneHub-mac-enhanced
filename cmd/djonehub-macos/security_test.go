package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureAPITokenPersistsWithPrivatePermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	token, err := ensureAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64", len(token))
	}
	path := filepath.Join(home, "Library", "Application Support", "DJOneHub", "api-token")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("token mode = %o, want 600", got)
	}
	reloaded, err := ensureAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded != token {
		t.Fatal("token changed across reload")
	}
}

func TestLocalSecurityRejectsUnauthenticatedAPI(t *testing.T) {
	a := &app{apiToken: strings.Repeat("a", 64)}
	handler := a.localSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7575/api/status", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestLocalSecurityAcceptsNativeToken(t *testing.T) {
	token := strings.Repeat("b", 64)
	a := &app{apiToken: token}
	handler := a.localSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7575/api/status", nil)
	request.Header.Set("X-DJOneHub-Token", token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestLocalSecurityRejectsCrossOriginAndPlainTextJSON(t *testing.T) {
	token := strings.Repeat("c", 64)
	a := &app{apiToken: token}
	handler := a.localSecurity(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	crossOrigin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7575/api/sms/send", strings.NewReader(`{"phone":"1"}`))
	crossOrigin.Header.Set("X-DJOneHub-Token", token)
	crossOrigin.Header.Set("Content-Type", "application/json")
	crossOrigin.Header.Set("Origin", "https://example.invalid")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, crossOrigin)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want %d", recorder.Code, http.StatusForbidden)
	}

	plainText := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7575/api/sms/send", strings.NewReader(`{"phone":"1"}`))
	plainText.Header.Set("X-DJOneHub-Token", token)
	plainText.Header.Set("Content-Type", "text/plain")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, plainText)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("plain-text status = %d, want %d", recorder.Code, http.StatusUnsupportedMediaType)
	}
}

func TestManualATAllowlist(t *testing.T) {
	for _, command := range []string{"AT", "AT+CSQ", `AT+QCFG="USBCFG"`, "AT+QGPSLOC=2"} {
		if !manualATCommandAllowed(command) {
			t.Fatalf("expected read-only command %q to be allowed", command)
		}
	}
	for _, command := range []string{"AT+CFUN=1,1", "ATD10086;", `AT+QCFG="USBCFG",1`, "AT+CSQ\nAT+CFUN=1"} {
		if manualATCommandAllowed(command) {
			t.Fatalf("expected mutating command %q to be rejected", command)
		}
	}
}
