package vk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/openlibrecommunity/olcrtc/internal/engine"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/pion/webrtc/v4"
)

const (
	defaultSendQueueSize = 5000
	maxReconnects        = 10
	reconnectWindow      = 5 * time.Minute
	pingInterval         = 15 * time.Second
	writeWait            = 10 * time.Second
)

var (
	ErrSessionClosed       = errors.New("vk session closed")
	ErrDataChannelNotReady = errors.New("vk datachannel not ready")
	ErrSendQueueFull       = errors.New("vk send queue full")
	ErrNoEndpoint          = errors.New("vk signaling endpoint missing")
)

type Session struct {
	name             string
	endpoint         string
	joinLink         string
	sessionKey       string
	applicationKey   string
	turnURLs         []string
	turnUsername     string
	turnCredential   string
	stunURLs         []string
	selfUserID       int64

	onData          func([]byte)
	onPeerData      func(peerID string, data []byte)
	onReconnect     func(*webrtc.DataChannel)
	shouldReconnect func() bool
	onEnded         func(string)
	refresh         func(ctx context.Context) (engine.Credentials, error)

	ws    *websocket.Conn
	wsMu  sync.Mutex
	pc    *webrtc.PeerConnection
	dc    *webrtc.DataChannel
	dcMu  sync.Mutex

	seq              int
	remotePeerID     *int64
	hasRemoteDesc    bool
	pendingLocalICE  []any
	pendingRemoteICE []webrtc.ICECandidateInit

	sendQueue      chan []byte
	reconnectCh    chan struct{}
	closeCh        chan struct{}
	closed         atomic.Bool
	isReconnecting atomic.Bool
	lastReconnect  time.Time
	reconnectCount int
	sessionMu      sync.Mutex
}

func New(_ context.Context, cfg engine.Config) (engine.Session, error) {
	endpoint := cfg.URL
	var turnURLs []string
	var stunURLs []string
	var turnUsername, turnCredential, sessionKey, applicationKey, joinLink string

	if cfg.Extra != nil {
		if u := cfg.Extra["endpoint"]; u != "" {
			endpoint = u
		}
		sessionKey = cfg.Extra["sessionKey"]
		applicationKey = cfg.Extra["applicationKey"]
		joinLink = cfg.Extra["joinLink"]
		turnUsername = cfg.Extra["turnUsername"]
		turnCredential = cfg.Extra["turnCredential"]
		if raw := cfg.Extra["turnURLs"]; raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if s := strings.TrimSpace(part); s != "" {
					turnURLs = append(turnURLs, s)
				}
			}
		}
		if raw := cfg.Extra["stunURLs"]; raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if s := strings.TrimSpace(part); s != "" {
					stunURLs = append(stunURLs, s)
				}
			}
		}
	}

	if endpoint == "" {
		endpoint = "wss://calls.okcdn.ru/ws"
	}

	s := &Session{
		name:           cfg.Name,
		endpoint:       endpoint,
		joinLink:       joinLink,
		sessionKey:     sessionKey,
		applicationKey: applicationKey,
		turnURLs:       turnURLs,
		turnUsername:   turnUsername,
		turnCredential: turnCredential,
		stunURLs:       stunURLs,
		onData:         cfg.OnData,
		onPeerData:     cfg.OnPeerData,
		refresh:        cfg.Refresh,
		sendQueue:      make(chan []byte, defaultSendQueueSize),
		reconnectCh:    make(chan struct{}, 1),
		closeCh:        make(chan struct{}),
	}

	return s, nil
}

func (s *Session) Capabilities() engine.Capabilities {
	return engine.Capabilities{ByteStream: true, VideoTrack: false}
}

func (s *Session) isSelf(peerID int64) bool {
	return s.selfUserID != 0 && peerID == s.selfUserID
}

func (s *Session) Connect(ctx context.Context) error {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	if s.closed.Load() {
		return ErrSessionClosed
	}

	endpoint := s.endpoint
	if !strings.Contains(endpoint, "appVersion=") {
		sep := "&"
		if !strings.Contains(endpoint, "?") {
			sep = "?"
		}
		endpoint = fmt.Sprintf("%s%sappVersion=1.127.0&device=0&version=2", endpoint, sep)
	}
	s.endpoint = endpoint

	if u, err := url.Parse(s.endpoint); err == nil {
		if uidStr := u.Query().Get("userId"); uidStr != "" {
			s.selfUserID, _ = strconv.ParseInt(uidStr, 10, 64)
		}
	}

	logger.Infof("Connecting to VK signaling endpoint: %s (selfUserID: %d)", s.endpoint, s.selfUserID)

	header := http.Header{}
	header.Set("Origin", "https://vk.ru")
	header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")

	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second

	ws, _, err := dialer.DialContext(ctx, s.endpoint, header)
	if err != nil {
		logger.Errorf("VK WS dial directly failed: %v", err)
		return fmt.Errorf("vk ws dial: %w", err)
	}
	s.wsMu.Lock()
	s.ws = ws
	s.wsMu.Unlock()

	if err := s.initPeerConnection(); err != nil {
		return fmt.Errorf("init peer connection: %w", err)
	}

	if s.ws != nil {
		go s.readLoop()
		go s.sendMediaSettings()
	}
	go s.sendLoop()

	return nil
}

func (s *Session) initPeerConnection() error {
	var iceServers []webrtc.ICEServer

	stunList := []string{
		"stun:stun.l.google.com:19302",
		"stun:stun1.l.google.com:19302",
	}
	if len(s.stunURLs) > 0 {
		stunList = append(s.stunURLs, stunList...)
	}
	iceServers = append(iceServers, webrtc.ICEServer{
		URLs: stunList,
	})

	if len(s.turnURLs) > 0 {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:           s.turnURLs,
			Username:       s.turnUsername,
			Credential:     s.turnCredential,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}

	cfg := webrtc.Configuration{
		ICEServers: iceServers,
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	s.pc = pc

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candVal := c.ToJSON()
		logger.Infof("VK local ICE candidate gathered: %s", candVal.Candidate)

		s.sessionMu.Lock()
		defer s.sessionMu.Unlock()
		if s.remotePeerID != nil {
			s.vkSend("transmit-data", map[string]any{
				"participantId": *s.remotePeerID,
				"data":          map[string]any{"candidate": candVal},
			})
		} else {
			s.pendingLocalICE = append(s.pendingLocalICE, candVal)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Infof("VK PeerConnection state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			if !s.isReconnecting.Load() && !s.closed.Load() {
				s.triggerReconnect("pc state: " + state.String())
			}
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		logger.Infof("VK remote DataChannel received: %s (id: %v)", dc.Label(), dc.ID())
		s.setupDataChannel(dc)
	})

	ordered := true
	maxRetransmits := uint16(0)
	localDC, err := pc.CreateDataChannel("data", &webrtc.DataChannelInit{
		Ordered:        &ordered,
		MaxRetransmits: &maxRetransmits,
	})
	if err != nil {
		logger.Debugf("VK create local datachannel error: %v", err)
	} else {
		s.setupDataChannel(localDC)
	}

	return nil
}

func (s *Session) setupDataChannel(dc *webrtc.DataChannel) {
	s.dcMu.Lock()
	s.dc = dc
	s.dcMu.Unlock()

	dc.OnOpen(func() {
		logger.Infof("VK DataChannel opened: %s", dc.Label())
		if s.onReconnect != nil {
			s.onReconnect(dc)
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if s.onData != nil {
			s.onData(msg.Data)
		}
	})

	dc.OnClose(func() {
		logger.Infof("VK DataChannel closed: %s", dc.Label())
	})
}

func (s *Session) sendMediaSettings() {
	s.vkSend("update-media-modifiers", map[string]any{
		"mediaModifiers": map[string]any{"denoise": true, "denoiseAnn": true},
	})
	s.vkSend("change-media-settings", map[string]any{
		"mediaSettings": map[string]any{
			"isAudioEnabled": false, "isVideoEnabled": true, "isScreenSharingEnabled": false, "isDataEnabled": true,
		},
	})
}

func (s *Session) readLoop() {
	s.wsMu.Lock()
	ws := s.ws
	s.wsMu.Unlock()
	if ws == nil {
		return
	}

	done := make(chan struct{})
	defer close(done)

	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-s.closeCh:
				return
			case <-done:
				return
			case <-ticker.C:
				s.wsMu.Lock()
				if s.ws == ws {
					_ = ws.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(writeWait))
				}
				s.wsMu.Unlock()
			}
		}
	}()

	for {
		if s.closed.Load() {
			return
		}

		_, msg, err := ws.ReadMessage()
		if err != nil {
			s.wsMu.Lock()
			isCurrent := (s.ws == ws)
			s.wsMu.Unlock()

			if isCurrent && !s.closed.Load() && !s.isReconnecting.Load() && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "use of closed network connection") {
				logger.Errorf("VK WS read error: %v", err)
				s.triggerReconnect("ws read error")
			}
			return
		}

		text := string(msg)
		if text == "ping" {
			s.wsMu.Lock()
			if s.ws == ws {
				_ = ws.WriteMessage(websocket.TextMessage, []byte("pong"))
			}
			s.wsMu.Unlock()
			continue
		}

		var data map[string]any
		if err := json.Unmarshal(msg, &data); err != nil {
			continue
		}

		s.handleSignalingMessage(data)
	}
}

func (s *Session) handleSignalingMessage(data map[string]any) {
	msgType, _ := data["type"].(string)
	eventName := msgType
	if n, ok := data["notification"].(string); ok && n != "" {
		eventName = n
	}

	switch eventName {
	case "connection":
		if conv, ok := data["conversation"].(map[string]any); ok {
			if parts, ok := conv["participants"].([]any); ok {
				for _, pAny := range parts {
					if pMap, ok := pAny.(map[string]any); ok {
						if pid, ok := pMap["id"].(float64); ok {
							peerID := int64(pid)
							if !s.isSelf(peerID) {
								s.sessionMu.Lock()
								if s.remotePeerID == nil {
									s.remotePeerID = &peerID
									logger.Infof("VK existing peer discovered in room: %d", peerID)
								}
								s.sessionMu.Unlock()
							}
						}
					}
				}
			}
		}

	case "conversation-participant-list":
		if parts, ok := data["participants"].([]any); ok {
			for _, pAny := range parts {
				if pMap, ok := pAny.(map[string]any); ok {
					if pid, ok := pMap["participantId"].(float64); ok {
						peerID := int64(pid)
						if !s.isSelf(peerID) {
							s.sessionMu.Lock()
							if s.remotePeerID == nil {
								s.remotePeerID = &peerID
								logger.Infof("VK existing peer discovered in participant-list: %d", peerID)
							}
							s.sessionMu.Unlock()
						}
					}
				}
			}
		}

	case "error":
		logger.Errorf("VK signaling error: %v (msg: %v)", data["error"], data["message"])

	case "registered-peer":
		if pid, ok := data["participantId"].(float64); ok {
			peerID := int64(pid)
			s.sessionMu.Lock()
			s.remotePeerID = &peerID
			s.sessionMu.Unlock()
			logger.Infof("VK remote peer registered: %d", peerID)
			go s.createAndSendOffer()
		}

	case "transmitted-data", "transmit-data":
		if pid, ok := data["participantId"].(float64); ok {
			peerID := int64(pid)
			s.sessionMu.Lock()
			if s.remotePeerID == nil || *s.remotePeerID != peerID {
				s.remotePeerID = &peerID
				logger.Infof("VK remote peer identified from transmit-data: %d", peerID)
			}
			s.sessionMu.Unlock()
		}

		innerData, ok := data["data"].(map[string]any)
		if !ok {
			return
		}

		if cand, ok := innerData["candidate"]; ok {
			candJSON, _ := json.Marshal(cand)
			var candInit webrtc.ICECandidateInit
			if json.Unmarshal(candJSON, &candInit) == nil && s.pc != nil {
				s.sessionMu.Lock()
				if s.hasRemoteDesc {
					err := s.pc.AddICECandidate(candInit)
					logger.Infof("VK added remote ICE candidate: %s (err: %v)", candInit.Candidate, err)
				} else {
					s.pendingRemoteICE = append(s.pendingRemoteICE, candInit)
					logger.Infof("VK queued remote ICE candidate: %s", candInit.Candidate)
				}
				s.sessionMu.Unlock()
			}
		}

		var sdpType, sdpStr string
		if sdpMap, ok := innerData["sdp"].(map[string]any); ok {
			sdpType, _ = sdpMap["type"].(string)
			sdpStr, _ = sdpMap["sdp"].(string)
		} else if t, ok := innerData["type"].(string); ok {
			sdpType = t
			sdpStr, _ = innerData["sdp"].(string)
		}
		if sdpType != "" && sdpStr != "" {
			logger.Infof("VK received SDP %s (%d bytes)", sdpType, len(sdpStr))
			go s.handleSDP(sdpType, sdpStr)
		}

	case "peer-left", "hungup":
		if pid, ok := data["participantId"].(float64); ok {
			logger.Infof("VK peer %d left", int64(pid))
			s.sessionMu.Lock()
			if s.remotePeerID != nil && *s.remotePeerID == int64(pid) {
				s.remotePeerID = nil
				s.hasRemoteDesc = false
			}
			s.sessionMu.Unlock()
		}
	}
}

func (s *Session) createAndSendOffer() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	if s.pc == nil || s.remotePeerID == nil {
		return
	}
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		logger.Debugf("VK create offer error: %v", err)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(offer); err != nil {
		logger.Debugf("VK set local description error: %v", err)
		return
	}

	select {
	case <-gatherComplete:
		logger.Infof("VK ICE gathering completed for offer")
	case <-time.After(3500 * time.Millisecond):
		logger.Infof("VK ICE gathering timeout for offer")
	}

	localDesc := s.pc.LocalDescription()
	if localDesc == nil {
		localDesc = &offer
	}

	s.vkSend("transmit-data", map[string]any{
		"participantId": *s.remotePeerID,
		"data": map[string]any{
			"sdp": map[string]any{
				"type": localDesc.Type.String(),
				"sdp":  localDesc.SDP,
			},
		},
	})
	logger.Infof("VK Offer sent to peer %d (SDP len: %d)", *s.remotePeerID, len(localDesc.SDP))

	for _, cand := range s.pendingLocalICE {
		s.vkSend("transmit-data", map[string]any{
			"participantId": *s.remotePeerID,
			"data":          map[string]any{"candidate": cand},
		})
	}
	s.pendingLocalICE = nil
}

func (s *Session) handleSDP(sdpType, sdpStr string) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	if s.pc == nil {
		return
	}

	if sdpType == "offer" {
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  sdpStr,
		}); err != nil {
			logger.Errorf("VK set remote offer error: %v", err)
			return
		}
		s.hasRemoteDesc = true
		s.flushPendingRemoteICE()

		answer, err := s.pc.CreateAnswer(nil)
		if err != nil {
			logger.Errorf("VK create answer error: %v", err)
			return
		}

		gatherComplete := webrtc.GatheringCompletePromise(s.pc)
		if err := s.pc.SetLocalDescription(answer); err != nil {
			logger.Errorf("VK set local answer error: %v", err)
			return
		}

		select {
		case <-gatherComplete:
			logger.Infof("VK ICE gathering completed for answer")
		case <-time.After(3500 * time.Millisecond):
			logger.Infof("VK ICE gathering timeout for answer")
		}

		localDesc := s.pc.LocalDescription()
		if localDesc == nil {
			localDesc = &answer
		}

		if s.remotePeerID != nil {
			s.vkSend("transmit-data", map[string]any{
				"participantId": *s.remotePeerID,
				"data": map[string]any{
					"sdp": map[string]any{
						"type": localDesc.Type.String(),
						"sdp":  localDesc.SDP,
					},
				},
			})
			logger.Infof("VK Answer sent to peer %d (SDP len: %d)", *s.remotePeerID, len(localDesc.SDP))
		}

		for _, cand := range s.pendingLocalICE {
			if s.remotePeerID != nil {
				s.vkSend("transmit-data", map[string]any{
					"participantId": *s.remotePeerID,
					"data":          map[string]any{"candidate": cand},
				})
			}
		}
		s.pendingLocalICE = nil
	} else if sdpType == "answer" {
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sdpStr,
		}); err != nil {
			logger.Errorf("VK set remote answer error: %v", err)
			return
		}
		s.hasRemoteDesc = true
		s.flushPendingRemoteICE()
	}
}

func (s *Session) flushPendingRemoteICE() {
	if s.pc == nil {
		return
	}
	for _, cand := range s.pendingRemoteICE {
		err := s.pc.AddICECandidate(cand)
		logger.Infof("VK flushed remote ICE candidate: %s (err: %v)", cand.Candidate, err)
	}
	s.pendingRemoteICE = nil
}

func (s *Session) vkSend(command string, extra map[string]any) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.ws == nil {
		return
	}

	s.seq++
	seq := s.seq

	var msgBytes []byte
	if pid, ok := extra["participantId"]; ok {
		dataJSON, _ := json.Marshal(extra["data"])
		ptype := "USER"
		if pt, ok := extra["participantType"].(string); ok && pt != "" {
			ptype = pt
		}
		msgBytes = []byte(fmt.Sprintf(`{"command":%q,"sequence":%d,"participantId":%v,"participantType":%q,"data":%s}`,
			command, seq, pid, ptype, dataJSON))
	} else {
		payload := map[string]any{
			"command":  command,
			"sequence": seq,
		}
		for k, v := range extra {
			payload[k] = v
		}
		var err error
		msgBytes, err = json.Marshal(payload)
		if err != nil {
			return
		}
	}

	_ = s.ws.WriteMessage(websocket.TextMessage, msgBytes)
}

func (s *Session) sendLoop() {
	for {
		select {
		case <-s.closeCh:
			return
		case data, ok := <-s.sendQueue:
			if !ok {
				return
			}
			s.dcMu.Lock()
			dc := s.dc
			s.dcMu.Unlock()
			if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
				if err := dc.Send(data); err != nil {
					logger.Debugf("VK DataChannel send error: %v", err)
				}
			}
		}
	}
}

func (s *Session) SetDataCallback(cb func([]byte)) {
	s.onData = cb
}

func (s *Session) SetPeerDataCallback(cb func(string, []byte)) {
	s.onPeerData = cb
}

func (s *Session) Send(data []byte) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}

	s.dcMu.Lock()
	dc := s.dc
	s.dcMu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrDataChannelNotReady
	}

	select {
	case s.sendQueue <- data:
		return nil
	case <-time.After(50 * time.Millisecond):
		return ErrSendQueueFull
	}
}

func (s *Session) CanSend() bool {
	s.dcMu.Lock()
	defer s.dcMu.Unlock()
	return s.dc != nil && s.dc.ReadyState() == webrtc.DataChannelStateOpen
}

func (s *Session) GetSendQueue() chan []byte {
	return s.sendQueue
}

func (s *Session) GetBufferedAmount() uint64 {
	s.dcMu.Lock()
	defer s.dcMu.Unlock()
	if s.dc != nil {
		return s.dc.BufferedAmount()
	}
	return 0
}

func (s *Session) SetReconnectCallback(cb func(*webrtc.DataChannel)) { s.onReconnect = cb }
func (s *Session) SetShouldReconnect(fn func() bool)                { s.shouldReconnect = fn }
func (s *Session) SetEndedCallback(cb func(string))                 { s.onEnded = cb }

func (s *Session) WatchConnection(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closeCh:
			return
		case <-s.reconnectCh:
			for {
				select {
				case <-s.reconnectCh:
				default:
					goto drained
				}
			}
		drained:
			if s.handleReconnect(ctx) {
				return
			}
		}
	}
}

func (s *Session) triggerReconnect(reason string) {
	logger.Infof("VK reconnect triggered: %s", reason)
	select {
	case s.reconnectCh <- struct{}{}:
	default:
	}
}

func (s *Session) Reconnect(reason string) {
	s.triggerReconnect(reason)
}

func (s *Session) handleReconnect(ctx context.Context) bool {
	s.isReconnecting.Store(true)
	defer s.isReconnecting.Store(false)

	if time.Since(s.lastReconnect) > reconnectWindow {
		s.reconnectCount = 0
	}
	s.reconnectCount++
	s.lastReconnect = time.Now()

	if s.reconnectCount > maxReconnects {
		if s.onEnded != nil {
			s.onEnded("reconnect limit reached")
		}
		return true
	}

	backoff := time.Duration(s.reconnectCount) * 2 * time.Second
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	select {
	case <-ctx.Done():
		return true
	case <-time.After(backoff):
	}

	if s.refresh != nil {
		fresh, err := s.refresh(ctx)
		if err == nil {
			s.endpoint = fresh.URL
			if fresh.Extra != nil {
				s.sessionKey = fresh.Extra["sessionKey"]
			}
		}
	}

	s.sessionMu.Lock()
	if s.ws != nil {
		_ = s.ws.Close()
		s.ws = nil
	}
	if s.pc != nil {
		_ = s.pc.Close()
		s.pc = nil
	}
	s.hasRemoteDesc = false
	s.remotePeerID = nil
	s.pendingLocalICE = nil
	s.pendingRemoteICE = nil
	s.sessionMu.Unlock()

	return s.Connect(ctx) != nil
}

func (s *Session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(s.closeCh)

	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()

	if s.ws != nil {
		_ = s.ws.Close()
		s.ws = nil
	}
	if s.pc != nil {
		_ = s.pc.Close()
		s.pc = nil
	}
	return nil
}

func init() {
	engine.Register("vk", New)
}
