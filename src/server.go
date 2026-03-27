package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"text/template"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

const (
	wsProtocol = "stream-updates"
)

type IngestInfo struct {
	streamerPeerConnection *webrtc.PeerConnection
	// trackLocals are the local tracks that map incoming stream ingest media
	// to outgoing watcher peer connections.
	localTracks           map[string]*webrtc.TrackLocalStaticRTP
	viewerPeerConnections map[uint64]*webrtc.PeerConnection
}

type Server struct {
	channels    *ChannelStore
	notifier    *Notifier
	webrtcAPI   *webrtc.API
	webrtcConfig webrtc.Configuration

	mu             sync.RWMutex
	streams        map[string]*IngestInfo
	nextViewerId   atomic.Uint64

	indexTemplate *template.Template
	watchTemplate *template.Template

	upgrader websocket.Upgrader
}

func NewServer(
	channels *ChannelStore,
	notifier *Notifier,
	webrtcAPI *webrtc.API,
	webrtcConfig webrtc.Configuration,
	indexTemplate *template.Template,
	watchTemplate *template.Template,
) *Server {
	return &Server{
		channels:      channels,
		notifier:      notifier,
		webrtcAPI:     webrtcAPI,
		webrtcConfig:  webrtcConfig,
		streams:       map[string]*IngestInfo{},
		indexTemplate: indexTemplate,
		watchTemplate: watchTemplate,
		upgrader: websocket.Upgrader{
			Subprotocols: []string{wsProtocol},
			CheckOrigin:  func(r *http.Request) bool { return true },
		},
	}
}

func (s *Server) writeCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Expose-Headers", "Location")
}

func (s *Server) HandleWebsocket(w http.ResponseWriter, r *http.Request) {
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("Websocket: Couldn't upgrade http connection", "err", err)
		return
	}
	defer c.Close()
	if c.Subprotocol() != wsProtocol {
		slog.Error("Websocket: Invalid subprotocol", "subprotocol", c.Subprotocol(),
			"expected", wsProtocol)
		return
	}
	uniqueId, receiveCh := s.notifier.Subscribe()
	// Remove from broadcast map and close channel before closing the connection.
	// This ensures no broadcaster can send to a closed channel.
	cleanup := sync.OnceFunc(func() {
		s.notifier.Unsubscribe(uniqueId)
	})
	defer cleanup()
	// Read goroutine: drains client messages and detects client-initiated close.
	// Closing the connection unblocks ReadMessage so this goroutine will exit.
	go func() {
		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				if _, ok := err.(*websocket.CloseError); ok {
					slog.Info("Websocket: Socket closed by client", "wsId", uniqueId)
				} else {
					slog.Error("Websocket: Receive error", "wsId", uniqueId, "err", err)
				}
				cleanup()
				return
			}
		}
	}()
	slog.Info("Websocket: Socket opened", "wsId", uniqueId)
	for notification := range receiveCh {
		err = c.WriteJSON(notification)
		if err != nil {
			slog.Error("Websocket: Could not send JSON data", "err", err)
			break
		}
	}
}

func (s *Server) HandleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	type ChannelData struct {
		Id     string
		IsLive bool
		Name   string
	}
	allChannels := s.channels.All()
	data := make([]ChannelData, len(allChannels))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range allChannels {
		_, isLive := s.streams[c.Id]
		data[i].Id = c.Id
		data[i].IsLive = isLive
		data[i].Name = c.Name
	}
	s.indexTemplate.Execute(w, data)
}

func (s *Server) HandleWatch(w http.ResponseWriter, r *http.Request) {
	channelId := r.PathValue("channelId")
	channel, err := s.channels.FindById(channelId)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	type WatchData struct {
		Id     string
		IsLive bool
		Name   string
	}
	_, isLive := s.streams[channelId]
	data := WatchData{}
	data.Id = channelId
	data.IsLive = isLive
	data.Name = channel.Name
	s.watchTemplate.Execute(w, data)
}

func (s *Server) HandleIngestStart(w http.ResponseWriter, r *http.Request) {
	s.writeCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Authenticate and determine the channel ID
	authRegex := regexp.MustCompile(`Bearer (\S+)`)
	authHeader := r.Header.Get("Authorization")
	authMatches := authRegex.FindStringSubmatch(authHeader)
	if len(authMatches) != 2 {
		slog.Error("Invalid authorization header")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	channelInfo, err := s.channels.FindByAuthKey(authMatches[1])
	if err != nil {
		slog.Error("Authorization header does not match any known channels")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	slog.Info("Received authenticated ingest request", "channel", channelInfo.Id)

	// Read the offer from HTTP Request
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Ingest: Could not read ingest HTTP content.")
		return
	}

	peerConnection, err := s.webrtcAPI.NewPeerConnection(s.webrtcConfig)
	if err != nil {
		slog.Error("Ingest: Could not create new peer connection", "error", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.streams[channelInfo.Id] = &IngestInfo{
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
			s.onIngestPeerConnectionClosed(channelInfo.Id)
		default:
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Create a track to fan out our incoming video to all peers
		trackLocal, err := s.addIngestTrack(channelInfo.Id, t)
		if err != nil {
			slog.Error("Ingest: Could not create local track", "error", err)
			return
		}
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
		slog.Error("Ingest: Could not set remote description", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		slog.Error("Ingest: Could not create answer", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		slog.Error("Ingest: Could not set local description", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	<-gatherComplete

	// WHIP+WHEP expects a Location header and a HTTP Status Code of 201
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/ingest/"+channelInfo.Id)
	w.WriteHeader(http.StatusCreated)

	// Write Answer with Candidates as HTTP Response
	fmt.Fprint(w, peerConnection.LocalDescription().SDP)
	slog.Info("Ingest: Accepted stream.", "channelId", channelInfo.Id)

	// Send websocket notification
	s.notifier.Broadcast(ChannelNotification{Id: channelInfo.Id, Name: channelInfo.Name, IsLive: true})
}

func (s *Server) HandleViewerStart(w http.ResponseWriter, r *http.Request) {
	s.writeCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		slog.Error("Viewer: WHEP start handler called with non-POST http method.")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	channelId := r.PathValue("channelId")
	if _, ok := s.streams[channelId]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	streamInfo := s.streams[channelId]

	// Read the offer from HTTP Request
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Viewer: Could not read HTTP content", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Create a new RTCPeerConnection
	peerConnection, err := s.webrtcAPI.NewPeerConnection(s.webrtcConfig)
	if err != nil {
		slog.Error("Viewer: Could not create peer connection", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	uniqueId := s.nextViewerId.Add(1)
	streamInfo.viewerPeerConnections[uniqueId] = peerConnection

	// Add tracks
	for _, t := range streamInfo.localTracks {
		if _, err := peerConnection.AddTrack(t); err != nil {
			slog.Error("Viewer: Could not add track", "error", err)
			peerConnection.Close()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
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
			s.mu.Lock()
			defer s.mu.Unlock()
			// If the channel stream still exists remove this connection from it
			if info, ok := s.streams[channelId]; ok {
				delete(info.viewerPeerConnections, uniqueId)
			}
		default:
		}
	})

	if err := peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		slog.Error("Viewer: Could not set remote description", "error", err)
		peerConnection.Close()
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		slog.Error("Viewer: Could not create answer", "error", err)
		peerConnection.Close()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		slog.Error("Viewer: Could not set local description", "error", err)
		peerConnection.Close()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	<-gatherComplete

	// WHIP+WHEP expects a Location header and a HTTP Status Code of 201
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/whep/"+channelId+"/"+strconv.FormatUint(uniqueId, 10))
	w.WriteHeader(http.StatusCreated)

	// Write Answer with Candidates as HTTP Response
	fmt.Fprint(w, peerConnection.LocalDescription().SDP)
	slog.Info("Viewer: Accepted stream.", "channelId", channelId, "peerConnectionId", uniqueId)
}

func (s *Server) HandleViewerStop(w http.ResponseWriter, r *http.Request) {
	s.writeCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.streams[channelId]; !ok {
		slog.Error("Viewer: WHEP stop handler called with non-existent channel ID.",
			"channelId", channelId)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if _, ok := s.streams[channelId].viewerPeerConnections[connectionId]; !ok {
		slog.Error("Viewer: WHEP stop handler called with non-existent connection ID.",
			"connectionId", connectionId)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err = s.streams[channelId].viewerPeerConnections[connectionId].Close(); err != nil {
		slog.Error("Viewer: Could not close peer connection", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) HandleIngestStop(w http.ResponseWriter, r *http.Request) {
	s.writeCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodDelete {
		slog.Error("Ingest: Stop handler called with non-DELETE http method.")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	channelId := r.PathValue("channelId")
	slog.Info("Ingest: Streamer requested stream stop.", "channelId", channelId)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.streams[channelId]; !ok {
		// Stream may have already been cleaned up by the connection state callback
		slog.Info("Ingest: Stream already stopped.", "channelId", channelId)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := s.streams[channelId].streamerPeerConnection.Close(); err != nil {
		slog.Error("Ingest: Could not close peer connection", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) onIngestPeerConnectionClosed(channelId string) {
	slog.Info("Ingest: Channel peer connection closed.", "channelId", channelId)
	channelInfo, err := s.channels.FindById(channelId)
	if err != nil {
		slog.Error("Ingest: Channel with unknown ID reported closed", "channelId", channelId)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Close all viewer connections
	slog.Info("Ingest: Closing viewer connections", "channelId", channelId,
		"numViewerConnections", len(s.streams[channelId].viewerPeerConnections))
	for _, c := range s.streams[channelId].viewerPeerConnections {
		c.Close()
	}

	delete(s.streams, channelId)

	// Send websocket notification
	s.notifier.Broadcast(ChannelNotification{Id: channelId, Name: channelInfo.Name, IsLive: false})
}

func (s *Server) addIngestTrack(channelId string, t *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a new TrackLocal
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(),
		t.StreamID())
	if err != nil {
		return nil, err
	}
	s.streams[channelId].localTracks[trackLocal.ID()] = trackLocal
	slog.Info("Ingest: Track added", "channel", channelId, "trackId", t.ID())

	// Update all viewers with new track
	if len(s.streams[channelId].viewerPeerConnections) > 0 {
		slog.Warn("Ingest track added while viewers are already connected; "+
			"existing viewers will not see the new track.", "channelId", channelId)
	}

	return trackLocal, nil
}
