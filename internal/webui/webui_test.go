package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAssetPathRejectsTraversal(t *testing.T) {
	for _, value := range []string{"/../index.html", "/assets/../../index.html", "/assets\\secret", "/assets/\x00secret"} {
		t.Run(value, func(t *testing.T) {
			if _, valid := assetPath(value); valid {
				t.Fatal("unsafe path accepted")
			}
		})
	}
}

func TestAssetPathAllowsRoutes(t *testing.T) {
	for _, value := range []string{"/", "/index.html", "/assets/app.js", "/settings/accounts"} {
		t.Run(value, func(t *testing.T) {
			if _, valid := assetPath(value); !valid {
				t.Fatal("safe route rejected")
			}
		})
	}
}

func TestHandlerRejectsEncodedTraversal(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/assets/%2e%2e/%2e%2e/index.html", nil)
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
}
