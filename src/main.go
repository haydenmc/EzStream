package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"text/template"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
)

var (
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
)

func main() {
	slog.Info("Hello!")

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

	// Set up media engine and interceptors
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}
	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		panic(err)
	}
	// Request periodic keyframes so new/recovering viewers get a clean frame quickly
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		panic(err)
	}
	interceptorRegistry.Add(intervalPliFactory)

	webrtcAPI := webrtc.NewAPI(
		webrtc.WithSettingEngine(settingsEngine),
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	)

	// Load JSON file
	content, err := os.ReadFile(*channelsJsonFilePath)
	if err != nil {
		slog.Error("Could not open streams JSON file", "file", channelsJsonFilePath)
		panic(err)
	}
	var channels []ChannelInfo
	err = json.Unmarshal(content, &channels)
	if err != nil {
		slog.Error("Could not parse JSON data")
		panic(err)
	}
	channelStore := NewChannelStore(channels)
	for _, c := range channels {
		slog.Info("Channel registered", "id", c.Id, "name", c.Name)
	}

	webrtcConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	srv := NewServer(channelStore, NewNotifier(), webrtcAPI, webrtcConfig,
		indexTemplate, watchTemplate)

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
	http.HandleFunc("/", srv.HandleIndex)
	http.HandleFunc("/watch/{channelId}", srv.HandleWatch)

	// WHIP+WHEP WebRTC endpoints
	http.HandleFunc("/ingest", srv.HandleIngestStart)
	http.HandleFunc("/ingest/{channelId}", srv.HandleIngestStop)
	http.HandleFunc("/whep/{channelId}", srv.HandleViewerStart)
	http.HandleFunc("/whep/{channelId}/{connectionId}", srv.HandleViewerStop)

	// Websocket API
	http.HandleFunc("/ws", srv.HandleWebsocket)

	slog.Info("Starting HTTP server...")
	http.ListenAndServe(*httpListenAddress, nil)
}
