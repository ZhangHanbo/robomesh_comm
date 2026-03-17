// Modified from rtmp-to-webrtc, to restream /dev/vide* media to WebRTC.
// Refer to README.md for example commandline.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"github.com/gorilla/websocket"

	"github.com/joho/godotenv"
)

func LoadConfig(path string, envFile string) (err error) {
	// (config Config, err error) {

	if err := godotenv.Load(envFile); err != nil {
		log.Printf("Info: No %s file found, relying on System Environment Variables\n", envFile)
	} else {
		log.Printf("Success: Loaded environment variables from %s\n", envFile)
	}

	return
}

// --- Chat and Signaling ---

const (
	MsgTypeChat      = 1   // User chat message
	MsgTypeOffer     = 100 // Inbound Offer from a new viewer
	MsgTypeCandidate = 101 // Inbound ICE Candidate from a viewer
	MsgTypeAnswer    = 102 // Outbound Answer to a viewer

	// Relay datachannel messages to viewers via websocket.
	MsgTypeRelayChat       = 200 // Outbound Relay Chat to all viewers
	MsgTypeRelayPointEvent = 201 // Outbound Relay Point Event to all viewers

	// Got turn server information from signaling server
	MsgTypeTurnInfo = 900
)

type Connection struct {
	Socket *websocket.Conn
	mu     sync.Mutex
}

type TurnServerInfo struct {
	URLs           []string `json:"urls"`
	Username       string   `json:"username"`
	Credential     string   `json:"credential"`
	CredentialType string   `json:"credentialType"`
}

var turnServerInfo TurnServerInfo

// Work around for sending messages back to the client.
type MessageType struct {
	Msgtype   int    `json:"msgtype"`
	Msgarg    string `json:"msgarg"`
	Msgsrc    string `json:"msgsrc"`
	Msgtext   string `json:"msgtext"`
	Msgts     int64  `json:"msgts"`
	Msgparam1 int    `json:"msgparam1"`
	Msgparam2 int    `json:"msgparam2"`
}

type ChatResponseType struct {
	Response string `json:"response"`
}

// Helper to convert our TurnServerInfo to webrtc.ICEServer
func (t TurnServerInfo) ToICEServers() []webrtc.ICEServer {
	// 1. Elegant Empty Check: If we have no URLs, return nothing.
	if len(t.URLs) == 0 {
		return nil
	}

	var targetType webrtc.ICECredentialType

	switch t.CredentialType {
	case "password":
		targetType = webrtc.ICECredentialTypePassword
	case "oauth":
		targetType = webrtc.ICECredentialTypeOauth
	default:
		// Default to password if undefined or empty
		targetType = webrtc.ICECredentialTypePassword
	}
	// 3. Return the properly constructed slice
	return []webrtc.ICEServer{
		{
			URLs:           t.URLs,
			Username:       t.Username,
			Credential:     t.Credential,
			CredentialType: targetType,
		},
	}
}

// PeerManager holds all active peer connections
type PeerManager struct {
	peers        map[string]*webrtc.PeerConnection
	dataChannels map[string]*webrtc.DataChannel // NEW: Map to store data channels
	mu           sync.RWMutex
}

// NewPeerManager creates a new PeerManager
func NewPeerManager() *PeerManager {
	return &PeerManager{
		peers:        make(map[string]*webrtc.PeerConnection),
		dataChannels: make(map[string]*webrtc.DataChannel),
	}
}

// AddPeer creates a new PeerConnection for a viewer
func (pm *PeerManager) AddPeer(
	uid string,
	videoTrack *webrtc.TrackLocalStaticSample,
	audioTrack *webrtc.TrackLocalStaticSample,
	videoRTCPConn *net.UDPConn,
	audioRTCPConn *net.UDPConn,
	ws *Connection, // WebSocket connection to send replies
	offer webrtc.SessionDescription,
	workstationIP string,
) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Check if peer already exists (e.g., reconnect)
	if pc, ok := pm.peers[uid]; ok {
		log.Printf("Peer %s already exists, closing old connection", uid)
		pc.Close()
		// Remove old data channel reference if it exists
		delete(pm.dataChannels, uid)
	}

	// Create a new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: turnServerInfo.ToICEServers(),
	})
	if err != nil {
		log.Printf("ERROR: Failed to create PeerConnection for %s: %v", uid, err)
		return
	}

	// Listen for datachannel offered by the client (browser)
	peerConnection.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("New DataChannel '%s' from peer %s", dc.Label(), uid)

		// Ensure this is the data channel we want
		if dc.Label() != "chat" {
			log.Printf("Ignoring data channel '%s'", dc.Label())
			return
		}

		// Store the data channel immediately
		pm.mu.Lock()
		pm.dataChannels[uid] = dc
		pm.mu.Unlock()

		// Register OnOpen handler
		dc.OnOpen(func() {
			log.Printf("Data channel 'chat' opened for peer %s", uid)
		})

		// Register OnMessage handler
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			log.Printf("Message from DataChannel 'chat' from peer %s: %s", uid, string(msg.Data))
			// Broadcast to all *other* peers
			// pm.BroadcastDataChannelMessage(uid, msg.Data)

			// Reply back via websocket channel for demo
			// Unmarshal message
			var usermessage MessageType
			if json.Unmarshal(msg.Data, &usermessage) == nil {

				switch usermessage.Msgtype {
				case 1:
					// relay message back for other spectators via websocket.
					sendMessageWS(ws, MsgTypeRelayChat, usermessage.Msgarg, usermessage.Msgsrc, usermessage.Msgtext, usermessage.Msgparam1, usermessage.Msgparam2)
					postBody, _ := json.Marshal(map[string]string{
						"text": usermessage.Msgtext,
					})
					responseBody := bytes.NewBuffer(postBody)
					response, err := http.Post("http://"+workstationIP+":11111/chat", "application/json", responseBody)
					defer response.Body.Close()
					body, err := ioutil.ReadAll(response.Body)
					if err != nil {
						log.Fatalf("An Error Occured %v", err)
					}
					// Unmarshal
					var chatResponse ChatResponseType
					json.Unmarshal(body, &chatResponse)
					// fmt.Println(chatResponse.Response)

					// sendMessageWS(connection, 1, "plain", "robi", chatResponse.Response, 0, 0)

					// fmt.Printf("Response: %v", repsonse)
					//Handle Error
					if err != nil {
						log.Fatalf("An Error Occured %v", err)
					}
					// FOR TEST: -- Pong back the same message to client, via websocket.
					// TODO: Please use this for sending any other message back
					// sendMessageWS(ws, 1, "plain", "robi", usermessage.Msgtext, 0, 0)
				case 10:
					// relay point event back for other spectators via websocket.
					sendMessageWS(ws, MsgTypeRelayPointEvent, usermessage.Msgarg, usermessage.Msgsrc, usermessage.Msgtext, usermessage.Msgparam1, usermessage.Msgparam2)
					fmt.Println("User message type 10")
					fmt.Println(usermessage.Msgarg)
					postBody, _ := json.Marshal(map[string]string{
						"point": usermessage.Msgarg,
					})
					responseBody := bytes.NewBuffer(postBody)
					_, err := http.Post("http://"+workstationIP+":11111/point", "application/json", responseBody)
					if err != nil {
						log.Fatalf("An Error Occured %v", err)
					}
				default:
					log.Printf("Unknown user message type")
				}
			}
		})

		// Register OnClose handler for cleanup
		dc.OnClose(func() {
			log.Printf("Data channel 'chat' closed for peer %s", uid)
			pm.mu.Lock()
			delete(pm.dataChannels, uid)
			pm.mu.Unlock()
		})
	})

	// Add tracks
	videoRtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		log.Printf("ERROR: Failed to add video track for %s: %v", uid, err)
		peerConnection.Close()
		return
	}
	processRTCP(videoRtpSender, videoRTCPConn)

	audioRtpSender, err := peerConnection.AddTrack(audioTrack)
	if err != nil {
		log.Printf("ERROR: Failed to add audio track for %s: %v", uid, err)
		peerConnection.Close()
		return
	}
	processRTCP(audioRtpSender, audioRTCPConn)

	// Set up ICE Candidate handler
	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		outboundCandidate, marshalErr := json.Marshal(c.ToJSON())
		if marshalErr != nil {
			log.Printf("ERROR: Failed to marshal ICE candidate for %s: %v", uid, marshalErr)
			return
		}

		// Send the candidate wrapped in MessageType
		sendMessageWS(ws, MsgTypeCandidate, "webrtc", uid, string(outboundCandidate), 0, 0)
	})

	// Set up Connection State handler (for removal)
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("Peer %s Connection State has changed %s \n", uid, connectionState.String())
		if connectionState == webrtc.ICEConnectionStateFailed ||
			connectionState == webrtc.ICEConnectionStateClosed ||
			connectionState == webrtc.ICEConnectionStateDisconnected {
			pm.RemovePeer(uid)
		}
	})

	// Set remote description (the offer)
	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		log.Printf("ERROR: Failed to set remote description for %s: %v", uid, err)
		peerConnection.Close()
		return
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Printf("ERROR: Failed to create answer for %s: %v", uid, err)
		peerConnection.Close()
		return
	}

	// Set local description
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		log.Printf("ERROR: Failed to set local description for %s: %v", uid, err)
		peerConnection.Close()
		return
	}

	// Send the answer wrapped in MessageType
	outboundAnswer, err := json.Marshal(answer)
	if err != nil {
		log.Printf("ERROR: Failed to marshal answer for %s: %v", uid, err)
		peerConnection.Close()
		return
	}

	sendMessageWS(ws, MsgTypeAnswer, "webrtc", uid, string(outboundAnswer), 0, 0)

	// Add to map
	pm.peers[uid] = peerConnection
	log.Printf("Successfully added peer %s", uid)
}

// AddICECandidate adds a received ICE candidate to the correct peer
func (pm *PeerManager) AddICECandidate(uid string, candidateStr string) {
	pm.mu.RLock()
	pc, ok := pm.peers[uid]
	pm.mu.RUnlock()

	if !ok {
		log.Printf("WARN: Received candidate for unknown peer %s", uid)
		return
	}

	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidateStr), &candidate); err != nil {
		log.Printf("ERROR: Failed to unmarshal candidate for %s: %v", uid, err)
		return
	}

	if err := pc.AddICECandidate(candidate); err != nil {
		log.Printf("ERROR: Failed to add ICE candidate for %s: %v", uid, err)
	}
}

// RemovePeer closes and removes a peer connection
func (pm *PeerManager) RemovePeer(uid string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pc, ok := pm.peers[uid]
	if !ok {
		return // Already removed
	}

	if err := pc.Close(); err != nil {
		log.Printf("ERROR: Failed to close PeerConnection for %s: %v", uid, err)
	}

	delete(pm.peers, uid)
	delete(pm.dataChannels, uid) // NEW: Clean up data channel reference
	log.Printf("Removed peer %s", uid)
}

// sends a message to all peers except the sender
func (pm *PeerManager) BroadcastDataChannelMessage(senderUID string, data []byte) {
	pm.mu.RLock() // Use RLock for reading the map
	defer pm.mu.RUnlock()

	for uid, dc := range pm.dataChannels {
		if uid == senderUID {
			continue // Don't send back to sender
		}

		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			if err := dc.Send(data); err != nil {
				log.Printf("ERROR: Failed to send data channel message to %s: %v", uid, err)
			}
		} else {
			log.Printf("WARN: Data channel for peer %s is not open (state: %s)", uid, dc.ReadyState().String())
		}
	}
}

// Concurrency handling - sending messages
func (c *Connection) Send(message []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Socket.WriteMessage(websocket.TextMessage, message)
}

func setupTCPServer(port int, connection *Connection) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("Error starting TCP server: %v", err)
	}
	defer listener.Close()

	log.Printf("Listening for incoming messages on port %d", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Fatalf("Error accepting connection: %v", err)
		}

		// Handle the incoming message in a separate goroutine
		go handleTCPConnection(conn, connection)
	}
}

func handleTCPConnection(conn net.Conn, connection *Connection) {
	defer conn.Close()

	// Read the incoming message
	messageBuffer := make([]byte, 1024)
	n, err := conn.Read(messageBuffer)
	if err != nil {
		log.Printf("Error reading from connection: %v", err)
		return
	}

	// Assuming the message is a string, convert it to a string
	message := string(messageBuffer[:n])
	log.Printf("Received message: %s", message)

	// Send the received message to the WebRTC peer through WebSocket
	if message != "end" {
		sendMessageWS(connection, 1, "plain", "robi", message, 0, 0)
	} else {
		sendMessageWS(connection, 1, "plain", "robi", "end", 1, 0)
	}
}

func sendMessageWS(connection *Connection, msgtype int, msgarg string, msgsrc string, msgstr string, msgparam1 int, msgparam2 int) {
	// Try sending back through the websocket.
	msgpack := MessageType{
		Msgtype:   msgtype,
		Msgarg:    msgarg,
		Msgsrc:    msgsrc, // Target User ID
		Msgtext:   msgstr,
		Msgts:     time.Now().Unix(),
		Msgparam1: msgparam1,
		Msgparam2: msgparam2,
	}
	msgsend, _ := json.Marshal(msgpack)
	if err := connection.Send(msgsend); err != nil {
		// client may be just disconnected.
		log.Printf("ERROR: Failed to send WebSocket message: %v", err)
	}
}

func NewStreamID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// Rare fallback to a timestamp or simple error string if /dev/urandom fails.
		log.Println("Error generating random ID:", err)
		return "fallback-id"
	}
	return hex.EncodeToString(b)
}

func main() {

	// 1. Parse command line flags
	envFile := flag.String("env", ".env", "Path to environment file (e.g., .env.prod, .env.sandbox)")
	flag.Parse()

	// 2. Load Config using it
	err := LoadConfig("./config", *envFile)
	if err != nil {
		log.Fatalf("cannot load config: %v", err)
	}

	WS_URL := os.Getenv("WS_URL")
	WS_PATH := os.Getenv("WS_PATH")
	WS_URLSCHEME := os.Getenv("WS_URLSCHEME")

	RTP_VIDEO_PORT, err := strconv.Atoi(os.Getenv("RTP_VIDEO_PORT"))
	if err != nil {
		log.Fatalf("Failed to parse RTP_VIDEO_PORT: %v", err)
		RTP_VIDEO_PORT = 5004
	}
	RTP_AUDIO_PORT, err := strconv.Atoi(os.Getenv("RTP_AUDIO_PORT"))
	if err != nil {
		log.Fatalf("Failed to parse RTP_AUDIO_PORT: %v", err)
		RTP_AUDIO_PORT = 5006
	}
	RTP_AUDIO_IP := os.Getenv("RTP_AUDIO_IP")
	RTP_VIDEO_IP := os.Getenv("RTP_VIDEO_IP")
	WORKSTATION_IP := os.Getenv("WORKSTATION_IP")

	// Exit signal
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// Set up streams.
	videoRTCPAddr, err := net.ResolveUDPAddr("udp", RTP_VIDEO_IP+":"+strconv.Itoa(RTP_VIDEO_PORT+1))
	if err != nil {
		log.Fatalf("Failed to resolve video RTCP address: %v", err)
	}
	videoRTCPConn, err := net.DialUDP("udp", nil, videoRTCPAddr)
	if err != nil {
		log.Fatalf("Failed to dial video RTCP: %v", err)
	}
	defer videoRTCPConn.Close()
	log.Printf("Forwarding Video RTCP to %s:%d", RTP_VIDEO_IP, RTP_VIDEO_PORT+1)

	audioRTCPAddr, err := net.ResolveUDPAddr("udp", RTP_AUDIO_IP+":"+strconv.Itoa(RTP_AUDIO_PORT+1))
	if err != nil {
		log.Fatalf("Failed to resolve audio RTCP address: %v", err)
	}
	audioRTCPConn, err := net.DialUDP("udp", nil, audioRTCPAddr)
	if err != nil {
		log.Fatalf("Failed to dial audio RTCP: %v", err)
	}
	defer audioRTCPConn.Close()
	log.Printf("Forwarding Audio RTCP to %s:%d", RTP_AUDIO_IP, RTP_AUDIO_PORT+1)

	// Connect Websocket.
	// b = broadcaster.
	u := url.URL{Scheme: WS_URLSCHEME, Host: WS_URL, Path: WS_PATH}
	q := u.Query()
	q.Add("role", os.Getenv("NODE_TYPE"))
	q.Add("token", os.Getenv("NODE_TOKEN"))
	q.Add("node_id", os.Getenv("NODE_ID"))
	u.RawQuery = q.Encode()

	log.Printf("connecting to %s", u.String())

	// Prepare websocket connection.
	wsconn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer wsconn.Close()

	connection := new(Connection)
	connection.Socket = wsconn

	// Start TCP server on a specified port, e.g., 8080
	go setupTCPServer(8080, connection)

	done := make(chan struct{})

	streamid := NewStreamID()
	log.Printf("Using Stream ID: %s", streamid)

	// Create a video track
	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "video/vp8"}, "video", streamid)
	if err != nil {
		panic(err)
	}

	// Create a video track
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", streamid)
	if err != nil {
		panic(err)
	}

	go rtpToTrack(videoTrack, &codecs.VP8Packet{}, 90000, RTP_VIDEO_IP, RTP_VIDEO_PORT)
	go rtpToTrack(audioTrack, &codecs.OpusPacket{}, 48000, RTP_AUDIO_IP, RTP_AUDIO_PORT)

	peerManager := NewPeerManager()

	// Start WebSocket read loop
	go func() {
		defer close(done)

		for {
			_, buf, err := wsconn.ReadMessage()
			if err != nil {
				log.Println("WebSocket read error:", err)
				return // Exit goroutine on read error
			}
			log.Printf("recv: %s", buf)

			// Unmarshal as a base MessageType to get routing info
			var message MessageType
			if err := json.Unmarshal(buf, &message); err != nil {
				log.Printf("WARN: Unknown message format: %v", err)
				continue
			}

			// Route message based on Msgtype
			switch message.Msgtype {
			case MsgTypeTurnInfo:
				// Got TURN server info from signaling server
				log.Printf("Received TURN server info: %s", message.Msgtext)
				var turnInfo TurnServerInfo
				if err := json.Unmarshal([]byte(message.Msgtext), &turnInfo); err != nil {
					log.Printf("ERROR: Failed to unmarshal TURN info: %v", err)
					continue
				}
				log.Printf("TURN URLs: %v", turnInfo.URLs)
			case MsgTypeOffer:
				// An offer from a new viewer
				log.Printf("Received Offer from %s", message.Msgsrc)
				var offer webrtc.SessionDescription
				if err := json.Unmarshal([]byte(message.Msgtext), &offer); err != nil {
					log.Printf("ERROR: Failed to unmarshal Offer SDP from %s: %v", message.Msgsrc, err)
					continue
				}
				// Pass all necessary components to create a new peer
				peerManager.AddPeer(message.Msgsrc, videoTrack, audioTrack, videoRTCPConn, audioRTCPConn, connection, offer, WORKSTATION_IP)

			case MsgTypeCandidate:
				// A candidate from an existing viewer
				log.Printf("Received Candidate from %s", message.Msgsrc)
				peerManager.AddICECandidate(message.Msgsrc, message.Msgtext)

			case MsgTypeChat:
				// A chat message (via WebSocket)
				log.Printf("Received WebSocket Chat from %s: %s", message.Msgsrc, message.Msgtext)
				// This is a WebSocket chat, not a WebRTC data channel chat.
				// You could broadcast this to all data channels if you want to bridge them:
				// peerManager.BroadcastDataChannelMessage("server-ws", []byte(message.Msgtext))

				// Or just echo back as you were:
				sendMessageWS(connection, MsgTypeChat, "plain", message.Msgsrc, message.Msgtext, 0, 0)

			default:
				log.Printf("WARN: Unknown message type received: %d", message.Msgtype)
			}
		}
	}()

	// Websocket end loop until Ctrl+C.
	for {
		select {
		case <-done:
			return
		case <-interrupt:
			log.Println("interrupt")

			// Cleanly close the connection by sending a close message and then
			// waiting (with timeout) for the server to close the connection.
			err := wsconn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				log.Println("write close:", err)
				return
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			return
		}
	}

}

// Listen for incoming packets on a port and write them to a Track
func rtpToTrack(track *webrtc.TrackLocalStaticSample, depacketizer rtp.Depacketizer, sampleRate uint32, ip string, port int) {
	// Open a UDP Listener for RTP Packets on port + 1
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(ip), Port: port})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err = listener.Close(); err != nil {
			log.Printf("ERROR: Failed to close listener: %v", err)
		}
	}()

	log.Printf("Waiting for RTP Packets on %s:%d\n", ip, port)
	sampleBuffer := samplebuilder.New(10, depacketizer, sampleRate)

	// Read RTP packets forever and send them to the WebRTC Client
	for {
		inboundRTPPacket := make([]byte, 1500) // UDP MTU
		packet := &rtp.Packet{}

		n, _, err := listener.ReadFrom(inboundRTPPacket)
		if err != nil {
			log.Printf("ERROR: Failed to read from UDP: %v", err)
			continue
		}

		if err = packet.Unmarshal(inboundRTPPacket[:n]); err != nil {
			log.Printf("ERROR: Failed to unmarshal RTP packet: %v", err)
			continue // just skip this packet
		}

		sampleBuffer.Push(packet)
		for {
			sample := sampleBuffer.Pop()
			if sample == nil {
				break
			}

			if writeErr := track.WriteSample(*sample); writeErr != nil {
				// this can happen if peers disconnect.
			}
		}
	}
}

// process
func processRTCP(rtpSender *webrtc.RTPSender, rtcpConn *net.UDPConn) {
	go func() {
		rtcpBuf := make([]byte, 1500)

		for {
			// Read RTCP packets from the browser
			n, _, rtcpErr := rtpSender.Read(rtcpBuf)
			if rtcpErr != nil {
				// This error is expected when the track is closed
				// log.Printf("RTCP Read Error: %v", rtcpErr)
				return
			}

			if rtcpConn == nil {
				// Safety check, in case something wasn't initialized
				continue
			}

			// Forward the RTCP packet to FFmpeg
			if _, writeErr := rtcpConn.Write(rtcpBuf[:n]); writeErr != nil {
				// this error can happen with the connection is closed
				// just let loop exit on the next Read error.

				// log.Printf("ERROR: Failed to forward RTCP packet: %v", writeErr)
			}
		}
	}()
}
