package vk

import (
	"os"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/auth"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

const (
	defaultAppKey     = "CGMMEJLGDIHBABABA"
	defaultAPIVersion = "5.282"
	defaultAppID      = "6287487"
	defaultAppVersion = "1.1"
	defaultUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"
)

// Provider produces VK Calls credentials for the VK engine.
type Provider struct{}

// Engine reports which engine consumes credentials from this auth provider.
func (Provider) Engine() string { return "vk" }

// DefaultServiceURL returns the base URL for VK calls.
func (Provider) DefaultServiceURL() string { return "https://vk.ru/call/join" }

// Issue resolves credentials for joining a VK call room.
func (Provider) Issue(ctx context.Context, cfg auth.Config) (auth.Credentials, error) {
	if cfg.RoomURL == "" {
		return auth.Credentials{}, auth.ErrRoomIDRequired
	}

	joinLink := ExtractJoinLink(cfg.RoomURL)
	if joinLink == "" {
		return auth.Credentials{}, fmt.Errorf("invalid vk call link: %q", cfg.RoomURL)
	}

	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		if u, err := url.Parse(cfg.RoomURL); err == nil {
			if q := u.Query().Get("token"); q != "" {
				token = q
			} else if q := u.Query().Get("call_token"); q != "" {
				token = q
			} else if q := u.Query().Get("auth_token"); q != "" {
				token = q
			} else if q := u.Query().Get("rp"); q != "" {
				token = q
			}
		}
	}

	info, err := Handshake(ctx, joinLink, cfg.Name, token)
	if err != nil {
		return auth.Credentials{}, fmt.Errorf("vk handshake: %w", err)
	}

	extra := map[string]string{
		"joinLink":       joinLink,
		"roomURL":        cfg.RoomURL,
		"sessionKey":     info.SessionKey,
		"applicationKey": info.ApplicationKey,
		"endpoint":       info.Endpoint,
		"wtEndpoint":     info.WtEndpoint,
		"turnURLs":       strings.Join(info.TurnServer.URLs, ","),
		"turnUsername":   info.TurnServer.Username,
		"turnCredential": info.TurnServer.Credential,
		"stunURLs":       strings.Join(info.StunServer.URLs, ","),
	}

	return auth.Credentials{
		URL:   info.Endpoint,
		Token: info.SessionKey,
		Extra: extra,
	}, nil
}

// HandshakeInfo stores the resolved VK call room metadata.
type HandshakeInfo struct {
	SessionKey     string
	ApplicationKey string
	Endpoint       string
	WtEndpoint     string
	TurnServer     struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username"`
		Credential string   `json:"credential"`
	}
	StunServer struct {
		URLs []string `json:"urls"`
	}
}

// ExtractJoinLink pulls the call token/ID from a VK call URL or raw ID.
func ExtractJoinLink(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if idx := strings.Index(raw, "call/join/"); idx >= 0 {
		raw = raw[idx+len("call/join/"):]
	}
	if idx := strings.Index(raw, "?"); idx >= 0 {
		raw = raw[:idx]
	}
	if idx := strings.Index(raw, "#"); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.Trim(raw, "/")
}

// Handshake contacts calls.okcdn.ru and retrieves the signaling endpoint and TURN credentials.
func Handshake(ctx context.Context, joinLink, displayName, userTokenOrCookie string) (*HandshakeInfo, error) {
	logger.Infof("VK Handshake: joinLink=%s displayName=%s tokenLen=%d tokenStart=%q", joinLink, displayName, len(userTokenOrCookie), func() string {
		if len(userTokenOrCookie) > 15 { return userTokenOrCookie[:15] }
		return userTokenOrCookie
	}())
	client := protect.NewHTTPClient()
	client.Timeout = 15 * time.Second

	httpPost := func(targetURL string, form url.Values, extraHeaders map[string]string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", defaultUserAgent)
		req.Header.Set("Origin", "https://vk.ru")
		req.Header.Set("Referer", "https://vk.ru/")
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}

	appKey := defaultAppKey
	var okAuthToken string
	var vkAccessToken string

	// Path A: Authenticated user mode via cookie or web_token
	tokenStr := strings.TrimSpace(userTokenOrCookie)
	if tokenStr == "" {
		if data, err := os.ReadFile("/etc/olcrtc/vk_cookies"); err == nil && len(data) > 0 {
			tokenStr = strings.TrimSpace(string(data))
			logger.Infof("VK Auth: loaded fallback cookies from /etc/olcrtc/vk_cookies")
		}
	}
	if tokenStr != "" {
		if strings.HasPrefix(tokenStr, "$") {
			okAuthToken = tokenStr
			logger.Infof("VK Auth: using direct call token")
		} else {

		if strings.HasPrefix(tokenStr, "vk1.a.") {
			vkAccessToken = tokenStr
		} else {
			// Format cookie string
			cookieStr := tokenStr
			if !strings.Contains(cookieStr, "=") {
				cookieStr = "remixsid=" + cookieStr
			}
			logger.Infof("VK Auth: requesting web_token with cookie...")
			webTokBytes, err := httpPost("https://login.vk.ru/?act=web_token", url.Values{
				"version": {"1"},
				"app_id":  {defaultAppID},
			}, map[string]string{
				"Cookie": cookieStr,
			})
			if err == nil {
				var webTok struct {
					Data struct {
						AccessToken string `json:"access_token"`
					} `json:"data"`
				}
				if json.Unmarshal(webTokBytes, &webTok) == nil && webTok.Data.AccessToken != "" {
					vkAccessToken = webTok.Data.AccessToken
					logger.Infof("VK Auth: successfully obtained fresh web_token for server IP")
				} else {
					logger.Debugf("VK Auth: web_token response: %s", string(webTokBytes))
				}
			}
		}

		if vkAccessToken != "" {
			// Get public key
			settingsBytes, err := httpPost("https://api.vk.ru/method/calls.getSettings", url.Values{
				"v": {defaultAPIVersion},
			}, map[string]string{
				"Authorization": "Bearer " + vkAccessToken,
			})
			if err == nil {
				var settings struct {
					Response struct {
						Settings struct {
							PublicKey string `json:"public_key"`
						} `json:"settings"`
					} `json:"response"`
				}
				if json.Unmarshal(settingsBytes, &settings) == nil && settings.Response.Settings.PublicKey != "" {
					appKey = settings.Response.Settings.PublicKey
				}
			}

			// Get call token
			callTokBytes, err := httpPost("https://api.vk.ru/method/messages.getCallToken", url.Values{
				"v":   {defaultAPIVersion},
				"env": {"production"},
			}, map[string]string{
				"Authorization": "Bearer " + vkAccessToken,
			})
			if err == nil {
				var callTok struct {
					Response struct {
						Token      string `json:"token"`
						APIBaseURL string `json:"api_base_url"`
					} `json:"response"`
				}
				if json.Unmarshal(callTokBytes, &callTok) == nil && callTok.Response.Token != "" {
					okAuthToken = callTok.Response.Token
					logger.Infof("VK Auth: successfully acquired call token")
				} else {
					logger.Debugf("VK Auth: messages.getCallToken response: %s", string(callTokBytes))
				}
			}
		}

		}

		if okAuthToken == "" && strings.HasPrefix(tokenStr, "vk1.a.") {
			if data, err := os.ReadFile("/etc/olcrtc/vk_cookies"); err == nil && len(data) > 0 {
				logger.Infof("VK Auth: access_token expired or rejected, falling back to /etc/olcrtc/vk_cookies...")
				cookieStr := strings.TrimSpace(string(data))
				webTokBytes, err := httpPost("https://login.vk.ru/?act=web_token", url.Values{
					"version": {"1"},
					"app_id":  {defaultAppID},
				}, map[string]string{
					"Cookie": cookieStr,
				})
				if err == nil {
					var webTok struct {
						Data struct {
							AccessToken string `json:"access_token"`
						} `json:"data"`
					}
					if json.Unmarshal(webTokBytes, &webTok) == nil && webTok.Data.AccessToken != "" {
						vkAccessToken = webTok.Data.AccessToken
						logger.Infof("VK Auth: obtained fresh web_token from fallback cookies")

						callTokBytes, _ := httpPost("https://api.vk.ru/method/messages.getCallToken", url.Values{
							"v":   {defaultAPIVersion},
							"env": {"production"},
						}, map[string]string{
							"Authorization": "Bearer " + vkAccessToken,
						})
						var callTok struct {
							Response struct {
								Token string `json:"token"`
							} `json:"response"`
						}
						if json.Unmarshal(callTokBytes, &callTok) == nil && callTok.Response.Token != "" {
							okAuthToken = callTok.Response.Token
							logger.Infof("VK Auth: successfully acquired call token using fallback")
						}
					}
				}
			}
		}
	}

	// Step 2: Login to OK calls API
	deviceID := fmt.Sprintf("%d", rand.Int63n(9e18))
	sdMap := map[string]any{
		"version":        2,
		"device_id":      deviceID,
		"client_version": defaultAppVersion,
		"client_type":    "SDK_JS",
	}
	if okAuthToken != "" {
		sdMap["version"] = 3
		sdMap["auth_token"] = okAuthToken
	}
	sessionData, _ := json.Marshal(sdMap)

	loginRespBytes, err := httpPost("https://calls.okcdn.ru/fb.do", url.Values{
		"method":          {"auth.anonymLogin"},
		"application_key": {appKey},
		"format":          {"json"},
		"session_data":    {string(sessionData)},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("anonymLogin: %w", err)
	}

	var loginResp struct {
		SessionKey string `json:"session_key"`
	}
	if err := json.Unmarshal(loginRespBytes, &loginResp); err != nil || loginResp.SessionKey == "" {
		return nil, fmt.Errorf("anonymLogin parse failed: %s", string(loginRespBytes))
	}
	sessionKey := loginResp.SessionKey

	// Step 3: Try to get anonymToken from VK API if not authorized session
	var anonymToken string
	if okAuthToken == "" {
		anonRespBytes, err := httpPost("https://login.vk.ru/?act=get_anonym_token", url.Values{
			"client_id": {defaultAppID},
		}, nil)
		if err == nil {
			var anonResp struct {
				Data struct {
					AccessToken string `json:"access_token"`
				} `json:"data"`
			}
			if json.Unmarshal(anonRespBytes, &anonResp) == nil && anonResp.Data.AccessToken != "" {
				name := displayName
				if name == "" {
					name = "Tester"
				}
				callRespBytes, err := httpPost("https://api.vk.ru/method/calls.getAnonymousToken", url.Values{
					"v":            {defaultAPIVersion},
					"vk_join_link": {"https://vk.ru/call/join/" + joinLink},
					"name":         {name},
				}, map[string]string{
					"Authorization": "Bearer " + anonResp.Data.AccessToken,
				})
				if err == nil {
					var callResp struct {
						Response struct {
							Token string `json:"token"`
						} `json:"response"`
					}
					if json.Unmarshal(callRespBytes, &callResp) == nil {
						anonymToken = callResp.Response.Token
					}
				}
			}
		}
	}

	// Step 4: Join conversation by link
	mediaSettings, _ := json.Marshal(map[string]bool{
		"isAudioEnabled":         false,
		"isVideoEnabled":         true,
		"isScreenSharingEnabled": false,
	})

	joinParams := url.Values{
		"method":          {"vchat.joinConversationByLink"},
		"session_key":     {sessionKey},
		"application_key": {appKey},
		"format":          {"json"},
		"joinLink":        {joinLink},
		"isVideo":         {"true"},
		"isAudio":         {"false"},
		"mediaSettings":   {string(mediaSettings)},
	}
	if anonymToken != "" {
		joinParams.Set("anonymToken", anonymToken)
	}

	joinRespBytes, err := httpPost("https://calls.okcdn.ru/fb.do", joinParams, nil)
	if err != nil {
		return nil, fmt.Errorf("joinConversationByLink: %w", err)
	}

	var joinResp struct {
		Endpoint   string `json:"endpoint"`
		WtEndpoint string `json:"wt_endpoint"`
		TurnServer struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"turn_server"`
		StunServer struct {
			URLs []string `json:"urls"`
		} `json:"stun_server"`
		ErrorCode int    `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}

	if err := json.Unmarshal(joinRespBytes, &joinResp); err != nil {
		return nil, fmt.Errorf("join parse failed: %w (body: %s)", err, string(joinRespBytes))
	}

	if joinResp.ErrorCode != 0 && joinResp.Endpoint == "" {
		return nil, fmt.Errorf("vk join error %d: %s", joinResp.ErrorCode, joinResp.ErrorMsg)
	}

	if joinResp.Endpoint == "" && joinResp.WtEndpoint == "" {
		return nil, fmt.Errorf("vk join returned no signaling endpoints: %s", string(joinRespBytes))
	}

	info := &HandshakeInfo{
		SessionKey:     sessionKey,
		ApplicationKey: appKey,
		Endpoint:       joinResp.Endpoint,
		WtEndpoint:     joinResp.WtEndpoint,
		TurnServer:     joinResp.TurnServer,
		StunServer:     joinResp.StunServer,
	}
	return info, nil
}

func init() {
	auth.Register("vk", Provider{})
}
