package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"text/template"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

const (
	wsProtocol = "stream-updates"
)

var (
	// HTTP/HTML
	//go:embed wwwroot/index.html.tmpl
	indexFileContents string
	indexTemplate     = template.Must(template.New("index").Parse(indexFileContents))
	//go:embed wwwroot/watch.html.tmpl
	watchFileContents string
	watchTemplate     = template.Must(template.New("watch").Parse(watchFileContents))
	//go:embed wwwroot/style.css
	styleFileContents string
	//go:embed wwwroot/Inter.var.woff2
	fontFileContents []byte

	// Channels
	channels []ChannelInfo

	// Websocket
	upgrader = websocket.Upgrader{Subprotocols: []string{wsProtocol},
		CheckOrigin: func(r *http.Request) bool {
			return true
		}}
	nextUniqueWsId uint64                              = 1
	wsChannels     map[uint64]chan ChannelNotification = map[uint64]chan ChannelNotification{}

	// WebRTC
	webRtcApi                          *webrtc.API
	webRtcDataLock                     sync.RWMutex
	ingestInfo                         map[string]*IngestInfo
	nextUniquePeerConnectionId         uint64
	defaultPeerConnectionConfiguration = webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
)

type ChannelNotification struct {
	Id     string
	Name   string
	IsLive bool
}

type ChannelInfo struct {
	Id      string
	Name    string
	AuthKey string
}

type IngestInfo struct {
	streamerPeerConnection *webrtc.PeerConnection
	// trackLocals are the local tracks that map incoming stream ingest media
	// to outgoing watcher peer connections.
	localTracks           map[string]*webrtc.TrackLocalStaticRTP
	viewerPeerConnections map[uint64]*webrtc.PeerConnection
}

func main() {
	slog.Info("Hello!")

	// Init globals
	ingestInfo = map[string]*IngestInfo{}
	nextUniquePeerConnectionId = 1

	// Parse flags
	slog.Info("Parsing flags...")
	channelsJsonFilePath := flag.String("channelsJsonFile", "channels.json",
		"Path to channels configuration JSON file")
	httpListenAddress := flag.String("httpListenAddress", ":8080",
		"Network address to listen for HTTP requests on.")
	minUdpPort := flag.Uint("minUdp", 20000, "Minimum UDP port for assigning WebRTC connections")
	maxUdpPort := flag.Uint("maxUdp", 21000, "Maximum UDP port for assigning WebRTC connections")
	networkInterface := flag.String("networkInterface", "", "Network interface to filter to")
	flag.Parse()
	slog.Info("Configuration JSON file", "path", *channelsJsonFilePath)
	slog.Info("HTTP listen address", "address", *httpListenAddress)
	slog.Info("UDP port range", "min", *minUdpPort, "max", *maxUdpPort)
	slog.Info("Network Interface", "interface", *networkInterface)

	settingsEngine := webrtc.SettingEngine{}
	settingsEngine.SetEphemeralUDPPortRange(uint16(*minUdpPort), uint16(*maxUdpPort))
	if *networkInterface != "" {
		settingsEngine.SetInterfaceFilter(func(s string) bool {
			if s == *networkInterface {
				return true
			} else {
				return false
			}
		})
	}
	// Filter to IPv4 only for now.
	settingsEngine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	webRtcApi = webrtc.NewAPI(webrtc.WithSettingEngine(settingsEngine))

	// Load JSON file
	content, err := os.ReadFile(*channelsJsonFilePath)
	if err != nil {
		slog.Error("Could not open streams JSON file", "file", channelsJsonFilePath)
		panic(err)
	}
	err = json.Unmarshal(content, &channels)
	if err != nil {
		slog.Error("Could not parse JSON data")
		panic(err)
	}
	for _, c := range channels {
		slog.Info("Channel registered", "id", c.Id, "name", c.Name)
	}

	// Set up HTTP endpoints
	// Serve some files straight from memory
	http.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, styleFileContents)
	})
	http.HandleFunc("/Inter.var.woff2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "font/woff2")
		w.WriteHeader(http.StatusOK)
		w.Write(fontFileContents)
	})

	// Templated HTML/front-end endpoints
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/watch/{channelId}", handleWatch)

	// WHIP+WHEP WebRTC endpoints
	http.HandleFunc("/ingest", handleIngestStart)
	http.HandleFunc("/ingest/{channelId}", handleIngestStop)
	http.HandleFunc("/whep/{channelId}", handleViewerStart)
	http.HandleFunc("/whep/{channelId}/{connectionId}", handleViewerStop)

	// Websocket API
	http.HandleFunc("/ws", handleWebsocket)

	slog.Info("Starting HTTP server...")
	http.ListenAndServe(*httpListenAddress, nil)
}

func handleWebsocket(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
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
	uniqueId := nextUniqueWsId
	nextUniqueWsId += 1
	receiveCh := make(chan ChannelNotification)
	wsChannels[uniqueId] = receiveCh
	defer delete(wsChannels, uniqueId)
	go func() {
		for {
			_, _, err := c.ReadMessage()
			if _, ok := err.(*websocket.CloseError); ok {
				slog.Info("Websocket: Socket closed by client", "wsId", uniqueId)
				close(receiveCh)
				return
			} else if err != nil {
				slog.Error("Websocket: Receive error", "wsId", uniqueId, "err", err)
				close(receiveCh)
				return
			}
		}
	}()
	slog.Info("Websocket: Socket opened", "wsId", uniqueId)
	for {
		channelUpdate, ok := <-receiveCh
		if !ok {
			slog.Info("Websocket: Channel closed", "wsId", uniqueId)
			break
		}
		err = c.WriteJSON(channelUpdate)
		if err != nil {
			slog.Error("Websocket: Could not marshal JSON data", "err", err)
			break
		}
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	type ChannelData struct {
		Id     string
		IsLive bool
		Name   string
	}
	data := make([]ChannelData, len(channels))
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	for i, c := range channels {
		_, isLive := ingestInfo[c.Id]
		data[i].Id = c.Id
		data[i].IsLive = isLive
		data[i].Name = c.Name
	}
	indexTemplate.Execute(w, data)
}

func handleWatch(w http.ResponseWriter, r *http.Request) {
	channelId := r.PathValue("channelId")
	channelFound := false
	var channel ChannelInfo
	for _, c := range channels {
		if c.Id == channelId {
			channel = c
			channelFound = true
			break
		}
	}
	if !channelFound {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	type WatchData struct {
		Id     string
		IsLive bool
		Name   string
	}
	_, isLive := ingestInfo[channelId]
	data := WatchData{}
	data.Id = channelId
	data.IsLive = isLive
	data.Name = channel.Name
	watchTemplate.Execute(w, data)
}

func findChannelInfoById(id string) (*ChannelInfo, error) {
	for _, c := range channels {
		if c.Id == id {
			return &c, nil
		}
	}
	return nil, errors.New("could not find channel with specified ID")
}

func findChannelInfoByAuthKey(authKey string) (*ChannelInfo, error) {
	for _, c := range channels {
		if c.AuthKey == authKey {
			return &c, nil
		}
	}
	return nil, errors.New("could not find channel with specified auth key")
}

func handleIngestStart(w http.ResponseWriter, r *http.Request) {
	// Authenticate and determine the channel ID
	authRegex := regexp.MustCompile(`Bearer (\S+)`)
	authHeader := r.Header.Get("Authorization")
	authMatches := authRegex.FindStringSubmatch(authHeader)
	if len(authMatches) != 2 {
		slog.Error("Invalid authorization header")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	channelInfo, err := findChannelInfoByAuthKey(authMatches[1])
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

	peerConnection, err := webRtcApi.NewPeerConnection(defaultPeerConnectionConfiguration)
	if err != nil {
		slog.Error("Ingest: Could not create new peer connection", "error", err)
		return
	}

	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()

	ingestInfo[channelInfo.Id] = &IngestInfo{
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
			onIngestPeerConnectionClosed(channelInfo.Id)
		default:
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Create a track to fan out our incoming video to all peers
		trackLocal := addIngestTrack(channelInfo.Id, t)
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
	w.Header().Add("Location", "/ingest/"+channelInfo.Id)
	w.WriteHeader(http.StatusCreated)

	// Write Answer with Candidates as HTTP Response
	if _, err = fmt.Fprint(w, peerConnection.LocalDescription().SDP); err != nil {
		panic(err)
	}
	slog.Info("Ingest: Accepted stream.", "channelId", channelInfo.Id)

	// Send websocket notification
	for _, wsc := range wsChannels {
		wsc <- ChannelNotification{Id: channelInfo.Id, Name: channelInfo.Name, IsLive: true}
	}
}

func handleViewerStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		slog.Error("Viewer: WHEP start handler called with non-POST http method.")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()
	channelId := r.PathValue("channelId")
	if _, ok := ingestInfo[channelId]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	streamInfo := ingestInfo[channelId]

	// Read the offer from HTTP Request
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webRtcApi.NewPeerConnection(defaultPeerConnectionConfiguration)
	if err != nil {
		panic(err)
	}
	uniqueId := nextUniquePeerConnectionId
	nextUniquePeerConnectionId += 1
	streamInfo.viewerPeerConnections[uniqueId] = peerConnection

	// Add tracks
	for _, t := range streamInfo.localTracks {
		_, err := peerConnection.AddTrack(t)
		if err != nil {
			panic(err)
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
			webRtcDataLock.Lock()
			defer webRtcDataLock.Unlock()
			// If the channel stream still exists remove this connection from it
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
	channelInfo, err := findChannelInfoById(channelId)
	if err != nil {
		slog.Error("Ingest: Channel with unknown ID reported closed", "channelId", channelId)
		return
	}
	webRtcDataLock.Lock()
	defer webRtcDataLock.Unlock()

	// Close all viewer connections
	slog.Info("Ingest: Closing viewer connections", "channelId", channelId,
		"numViewerConnections", len(ingestInfo[channelId].viewerPeerConnections))
	for _, c := range ingestInfo[channelId].viewerPeerConnections {
		c.Close()
	}

	delete(ingestInfo, channelId)

	// Send websocket notification
	for _, wsc := range wsChannels {
		wsc <- ChannelNotification{Id: channelId, Name: channelInfo.Name, IsLive: false}
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
	if len(ingestInfo[channelId].viewerPeerConnections) > 0 {
		slog.Warn("Ingest track added while viewers are already connected; "+
			"existing viewers will not see the new track.", "channelId", channelId)
	}

	return trackLocal
}
