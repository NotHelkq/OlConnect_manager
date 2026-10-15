package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/openlibrecommunity/olcrtc/internal/auth/vk"
)

func main() {
	link := "AY_5KMQDmgIhQJkD7a2_6ykeljKVk1u2l68avLngyh0"
	cookies, _ := os.ReadFile("/etc/olcrtc/vk_cookies")
	info, err := vk.Handshake(context.Background(), link, "TestPeer", string(cookies))
	if err != nil {
		fmt.Println("Handshake error:", err)
		return
	}
	wsURL := info.Endpoint + "&appVersion=1.127.0&device=0&version=2"
	fmt.Printf("Connecting to: %s\n", wsURL)

	header := http.Header{}
	header.Set("Origin", "https://vk.ru")
	header.Set("User-Agent", "Mozilla/5.0")
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		fmt.Println("Dial err:", err)
		return
	}
	defer ws.Close()

	// Send media modifiers like normal client
	ws.WriteMessage(websocket.TextMessage, []byte(`{"command":"update-media-modifiers","sequence":2,"mediaModifiers":{"denoise":true,"denoiseAnn":true}}`))
	ws.WriteMessage(websocket.TextMessage, []byte(`{"command":"change-media-settings","sequence":3,"mediaSettings":{"isAudioEnabled":false,"isVideoEnabled":true,"isScreenSharingEnabled":false}}`))

	for i := 0; i < 10; i++ {
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, msg, err := ws.ReadMessage()
		if err != nil {
			fmt.Println("Read err:", err)
			break
		}
		fmt.Printf("INCOMING [%d]: %s\n", i, string(msg))
	}
}
