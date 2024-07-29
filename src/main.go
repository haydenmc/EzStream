package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"text/template"

	"github.com/pion/webrtc/v4"
)

var (
	// HTTP/HTML
	indexTemplate = template.Must(template.ParseFiles("index.html.tmpl"))

	// WebRTC
	webRtcDataLock        sync.RWMutex
	ingestInfo            map[string]*IngestInfo
	viewerPeerConnections map[string][]*webrtc.PeerConnection
)

type IngestInfo struct {
	peerConnection *webrtc.PeerConnection
	// trackLocals are the local tracks that map incoming stream ingest media
	// to outgoing watcher peer connections.
	localTracks map[string]*webrtc.TrackLocalStaticRTP
}

func main() {
	slog.Info("Hello!")

	ingestInfo = map[string]*IngestInfo{}
	viewerPeerConnections = map[string][]*webrtc.PeerConnection{}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/ingest", handleIngestStart)
	http.HandleFunc("/ingest/{channelId}", handleIngestStop)
	slog.Info("Starting HTTP server...")
	http.ListenAndServe(":8080", nil)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	indexTemplate.Execute(w, nil)
}

func handleIngestStart(w http.ResponseWriter, r *http.Request) {
	// TODO - eventually we'll pull this out of an authorization header
	channelId := "1"

	// Read the offer from HTTP Request
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Ingest: Could not read ingest HTTP content.")
		return
	}

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		slog.Error("Ingest: Could not create new peer connection", "error", err)
		return
	}

	ingestInfo[channelId] = &IngestInfo{peerConnection: peerConnection,
		localTracks: map[string]*webrtc.TrackLocalStaticRTP{}}

	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := peerConnection.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			slog.Error("Ingest: Could not populate peer connection transceivers", "error", err)
		}
	}

	// If PeerConnection is closed remove it from global list
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				slog.Error("Ingest: Peer connection failure + close error", "error", err)
			}
		case webrtc.PeerConnectionStateClosed:
			onIngestPeerConnectionClosed(channelId)
		default:
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Create a track to fan out our incoming video to all peers
		trackLocal := addIngestTrack(channelId, t)
		defer removeIngestTrack(channelId, trackLocal)

		buf := make([]byte, 1500)
		for {
			i, _, err := t.Read(buf)
			if err != nil {
				return
			}

			if _, err = trackLocal.Write(buf[:i]); err != nil {
				return
			}
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		slog.Info("Ingest: ICE Connection State has changed.", "state", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateFailed {
			_ = peerConnection.Close()
		}
	})

	if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// WHIP+WHEP expects a Location header and a HTTP Status Code of 201
	w.Header().Add("Location", "/ingest/"+channelId)
	w.WriteHeader(http.StatusCreated)

	// Write Answer with Candidates as HTTP Response
	if _, err = fmt.Fprint(w, peerConnection.LocalDescription().SDP); err != nil {
		slog.Error("Ingest: Error writing http response", "error", err)
	}
	slog.Info("Ingest: Accepted stream.", "channelId", channelId)
}

func onIngestPeerConnectionClosed(channelId string) {
	slog.Info("Ingest: Channel peer connection closed.", "channelId", channelId)
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()

	delete(ingestInfo, channelId)
}

func handleIngestStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		slog.Error("Ingest: Stop handler called with non-DELETE http method.")
		return
	}
	channelId := r.PathValue("channelId")
	slog.Info("Ingest: Streamer requested stream stop.", "channelId", channelId)
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	if _, ok := ingestInfo[channelId]; ok {
		ingestInfo[channelId].peerConnection.Close()
	}
}

func addIngestTrack(channelId string, t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()

	// Create a new TrackLocal
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(),
		t.StreamID())
	if err != nil {
		panic(err)
	}
	ingestInfo[channelId].localTracks[trackLocal.ID()] = trackLocal
	slog.Info("Ingest: Track added", "channel", channelId, "trackId", t.ID())

	// Update all viewers with new track
	for _, p := range viewerPeerConnections[channelId] {
		p.AddTrack(trackLocal)
	}

	return trackLocal
}

// Remove from list of tracks and fire renegotation for all PeerConnections
func removeIngestTrack(channelId string, t *webrtc.TrackLocalStaticRTP) {
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	for _, p := range viewerPeerConnections[channelId] {
		senders := p.GetSenders()
		for _, s := range senders {
			if s.Track() == t {
				p.RemoveTrack(s)
				break
			}
		}
	}
	delete(ingestInfo[channelId].localTracks, t.ID())
	slog.Info("Ingest: Track removed", "channel", channelId, "trackId", t.ID())
}
