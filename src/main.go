package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"text/template"

	"github.com/pion/webrtc/v4"
)

var (
	// HTTP/HTML
	indexTemplate = template.Must(template.ParseFiles("index.html.tmpl"))

	// WebRTC
	webRtcDataLock                     sync.RWMutex
	ingestInfo                         map[string]*IngestInfo
	uniquePeerConnectionId             uint64
	defaultPeerConnectionConfiguration = webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
)

type IngestInfo struct {
	streamerPeerConnection *webrtc.PeerConnection
	// trackLocals are the local tracks that map incoming stream ingest media
	// to outgoing watcher peer connections.
	localTracks           map[string]*webrtc.TrackLocalStaticRTP
	viewerPeerConnections map[uint64]*webrtc.PeerConnection
}

func main() {
	slog.Info("Hello!")

	ingestInfo = map[string]*IngestInfo{}
	uniquePeerConnectionId = 1

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/ingest", handleIngestStart)
	http.HandleFunc("/ingest/{channelId}", handleIngestStop)
	http.HandleFunc("/whep/{channelId}", handleViewerStart)
	http.HandleFunc("/whep/{channelId}/{connectionId}", handleViewerStop)
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

	ingestInfo[channelId] = &IngestInfo{
		streamerPeerConnection: peerConnection,
		localTracks:            map[string]*webrtc.TrackLocalStaticRTP{},
		viewerPeerConnections:  map[uint64]*webrtc.PeerConnection{}}

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
	<-gatherComplete

	// WHIP+WHEP expects a Location header and a HTTP Status Code of 201
	w.Header().Add("Location", "/ingest/"+channelId)
	w.WriteHeader(http.StatusCreated)

	// Write Answer with Candidates as HTTP Response
	if _, err = fmt.Fprint(w, peerConnection.LocalDescription().SDP); err != nil {
		panic(err)
	}
	slog.Info("Ingest: Accepted stream.", "channelId", channelId)
}

func handleViewerStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.Error("Viewer: WHEP start handler called with non-POST http method.")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	channelId := r.PathValue("channelId")
	if _, ok := ingestInfo[channelId]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	streamInfo := ingestInfo[channelId]

	// Read the offer from HTTP Request
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(defaultPeerConnectionConfiguration)
	if err != nil {
		panic(err)
	}
	uniqueId := uniquePeerConnectionId
	uniquePeerConnectionId += 1
	streamInfo.viewerPeerConnections[uniqueId] = peerConnection

	// Add tracks
	for _, t := range streamInfo.localTracks {
		rtpSender, err := peerConnection.AddTrack(t)
		if err != nil {
			panic(err)
		}
		// Read incoming RTCP packets
		// Before these packets are returned they are processed by interceptors. For things
		// like NACK this needs to be called.
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}()
	}

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		slog.Info("Viewer: ICE Connection State has changed.", "state", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateFailed {
			_ = peerConnection.Close()
		}
	})

	// If PeerConnection is closed remove it from global list
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				slog.Error("Viewer: Peer connection failure + close error", "error", err)
			}
		case webrtc.PeerConnectionStateClosed:
			slog.Info("Viewer: Peer connection closed.", "channelId", channelId,
				"uniqueId", uniqueId)
			webRtcDataLock.Lock()
			defer webRtcDataLock.Unlock()
			if info, ok := ingestInfo[channelId]; ok {
				delete(info.viewerPeerConnections, uniqueId)
			}
		default:
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
	<-gatherComplete

	// WHIP+WHEP expects a Location header and a HTTP Status Code of 201
	w.Header().Add("Location", "/whep/"+channelId+"/"+strconv.FormatUint(uniqueId, 10))
	w.WriteHeader(http.StatusCreated)

	// Write Answer with Candidates as HTTP Response
	if _, err = fmt.Fprint(w, peerConnection.LocalDescription().SDP); err != nil {
		panic(err)
	}
	slog.Info("Viewer: Accepted stream.", "channelId", channelId, "peerConnectionId", uniqueId)
}

func handleViewerStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		slog.Error("Viewer: WHEP stop handler called with non-DELETE http method.")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	channelId := r.PathValue("channelId")
	connectionId, err := strconv.ParseUint(r.PathValue("connectionId"), 10, 64)
	if err != nil {
		slog.Error("Viewer: Could not parse connectionId")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	slog.Info("Viewer: Requested stream stop.", "channelId", channelId, "peerConnectionId",
		connectionId)
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	if _, ok := ingestInfo[channelId]; !ok {
		slog.Error("Viewer: WHEP stop handler called with non-existent channel ID.",
			"channelId", channelId)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if _, ok := ingestInfo[channelId].viewerPeerConnections[connectionId]; !ok {
		slog.Error("Viewer: WHEP stop handler called with non-existent connection ID.",
			"connectionId", connectionId)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err = ingestInfo[channelId].viewerPeerConnections[connectionId].Close(); err != nil {
		panic(err)
	}
	w.WriteHeader(http.StatusOK)
}

func handleIngestStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		slog.Error("Ingest: Stop handler called with non-DELETE http method.")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	channelId := r.PathValue("channelId")
	slog.Info("Ingest: Streamer requested stream stop.", "channelId", channelId)
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	if _, ok := ingestInfo[channelId]; !ok {
		slog.Error("Ingest: Stop handler called with non-existent channel ID.",
			"channelId", channelId)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := ingestInfo[channelId].streamerPeerConnection.Close(); err != nil {
		panic(err)
	}
	w.WriteHeader(http.StatusOK)
}

func onIngestPeerConnectionClosed(channelId string) {
	slog.Info("Ingest: Channel peer connection closed.", "channelId", channelId)
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()

	delete(ingestInfo, channelId)
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
	for _, p := range ingestInfo[channelId].viewerPeerConnections {
		p.AddTrack(trackLocal)
	}

	return trackLocal
}

func removeIngestTrack(channelId string, t *webrtc.TrackLocalStaticRTP) {
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	for _, p := range ingestInfo[channelId].viewerPeerConnections {
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
