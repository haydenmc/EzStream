package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"text/template"

	"github.com/pion/webrtc/v4"
)

func testServer() *Server {
	channels := NewChannelStore([]ChannelInfo{
		{Id: "chan1", Name: "Channel One", AuthKey: "secret1"},
		{Id: "chan2", Name: "Channel Two", AuthKey: "secret2"},
	})
	indexTmpl := template.Must(template.New("index").Parse("{{range .}}{{.Id}} {{.Name}} {{.IsLive}}\n{{end}}"))
	watchTmpl := template.Must(template.New("watch").Parse("{{.Id}} {{.Name}} {{.IsLive}}"))
	webrtcConfig := webrtc.Configuration{}

	return NewServer(channels, NewNotifier(), webrtc.NewAPI(), webrtcConfig,
		indexTmpl, watchTmpl)
}

func TestHandleIndex_OK(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.HandleIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "chan1") || !strings.Contains(body, "chan2") {
		t.Fatalf("expected channel IDs in body, got: %s", body)
	}
}

func TestHandleIndex_NotFoundForOtherPaths(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.HandleIndex(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleWatch_OK(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/watch/chan1", nil)
	req.SetPathValue("channelId", "chan1")
	w := httptest.NewRecorder()
	srv.HandleWatch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "chan1") || !strings.Contains(body, "Channel One") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandleWatch_NotFound(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/watch/nonexistent", nil)
	req.SetPathValue("channelId", "nonexistent")
	w := httptest.NewRecorder()
	srv.HandleWatch(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleIngestStart_MethodNotAllowed(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/ingest", nil)
	w := httptest.NewRecorder()
	srv.HandleIngestStart(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleIngestStart_NoAuth(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader("fake-sdp"))
	w := httptest.NewRecorder()
	srv.HandleIngestStart(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleIngestStart_BadAuth(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader("fake-sdp"))
	req.Header.Set("Authorization", "Bearer wrongkey")
	w := httptest.NewRecorder()
	srv.HandleIngestStart(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandleIngestStop_MethodNotAllowed(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/ingest/chan1", nil)
	req.SetPathValue("channelId", "chan1")
	w := httptest.NewRecorder()
	srv.HandleIngestStop(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleIngestStop_AlreadyStopped(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodDelete, "/ingest/chan1", nil)
	req.SetPathValue("channelId", "chan1")
	w := httptest.NewRecorder()
	srv.HandleIngestStop(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestHandleViewerStart_MethodNotAllowed(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/whep/chan1", nil)
	req.SetPathValue("channelId", "chan1")
	w := httptest.NewRecorder()
	srv.HandleViewerStart(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleViewerStart_NotFound(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodPost, "/whep/chan1", strings.NewReader("fake-sdp"))
	req.SetPathValue("channelId", "chan1")
	w := httptest.NewRecorder()
	srv.HandleViewerStart(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleViewerStop_MethodNotAllowed(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/whep/chan1/1", nil)
	req.SetPathValue("channelId", "chan1")
	req.SetPathValue("connectionId", "1")
	w := httptest.NewRecorder()
	srv.HandleViewerStop(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHandleViewerStop_BadConnectionId(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodDelete, "/whep/chan1/notanumber", nil)
	req.SetPathValue("channelId", "chan1")
	req.SetPathValue("connectionId", "notanumber")
	w := httptest.NewRecorder()
	srv.HandleViewerStop(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleViewerStop_NotFound(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodDelete, "/whep/chan1/1", nil)
	req.SetPathValue("channelId", "chan1")
	req.SetPathValue("connectionId", "1")
	w := httptest.NewRecorder()
	srv.HandleViewerStop(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandlerCORS_Preflight(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodOptions, "/ingest", nil)
	w := httptest.NewRecorder()
	srv.HandleIngestStart(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("expected CORS Allow-Origin header")
	}
}

func TestHandlerCORS_HeadersOnNormalRequest(t *testing.T) {
	srv := testServer()
	req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader("fake-sdp"))
	w := httptest.NewRecorder()
	srv.HandleIngestStart(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("expected CORS Allow-Origin header on non-preflight request")
	}
}
