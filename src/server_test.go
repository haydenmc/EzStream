package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
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

// postSDP creates an offer on pc, sends it through the given handler and
// applies the answer.
func postSDP(t *testing.T, pc *webrtc.PeerConnection, handler http.HandlerFunc,
	prepare func(r *http.Request)) {
	t.Helper()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gatherComplete

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(pc.LocalDescription().SDP))
	prepare(req)
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: w.Body.String()}); err != nil {
		t.Fatal(err)
	}
}

func TestViewerKeyframeRequestReachesStreamer(t *testing.T) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	srv := testServer()
	srv.webrtcAPI = webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	// Streamer: publish a video track and keep RTP flowing.
	streamer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer streamer.Close()
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "stream")
	if err != nil {
		t.Fatal(err)
	}
	streamerSender, err := streamer.AddTrack(track)
	if err != nil {
		t.Fatal(err)
	}
	postSDP(t, streamer, srv.HandleIngestStart, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer secret1")
	})

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for seq := uint16(0); ; seq++ {
			select {
			case <-done:
				return
			case <-ticker.C:
				track.WriteRTP(&rtp.Packet{
					Header: rtp.Header{Version: 2, SequenceNumber: seq,
						Timestamp: uint32(seq) * 3000},
					Payload: []byte{0x41, 0x00},
				})
			}
		}
	}()

	// Wait for the server to register the ingest track.
	deadline := time.Now().Add(10 * time.Second)
	for {
		srv.mu.RLock()
		numTracks := len(srv.streams["chan1"].localTracks)
		srv.mu.RUnlock()
		if numTracks > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for ingest track")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Viewer: request a keyframe as soon as media arrives.
	viewer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	if _, err := viewer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	viewer.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		viewer.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{MediaSSRC: uint32(remote.SSRC())},
		})
	})
	postSDP(t, viewer, srv.HandleViewerStart, func(r *http.Request) {
		r.SetPathValue("channelId", "chan1")
	})

	gotPLI := make(chan struct{})
	go func() {
		for {
			packets, _, err := streamerSender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range packets {
				if _, ok := p.(*rtcp.PictureLossIndication); ok {
					close(gotPLI)
					return
				}
			}
		}
	}()

	select {
	case <-gotPLI:
	case <-time.After(10 * time.Second):
		t.Fatal("streamer never received the viewer's keyframe request")
	}
}
