package main

import (
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
)

func main() {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{
					"stun:stun.l.google.com:19302",
					"stun:90.156.234.250:19302",
				},
			},
			{
				URLs: []string{
					"turn:90.156.234.250:19302",
					"turn:95.163.34.172:19302",
				},
				Username:       "1792064526:1125900250830554",
				Credential:     "cYCuEXx0PzDXiDv+ZaycpndLpaU=",
				CredentialType: webrtc.ICECredentialTypePassword,
			},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		fmt.Printf("NewPeerConnection err: %v\n", err)
		return
	}
	defer pc.Close()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			fmt.Printf("Gathered candidate: %s\n", c.ToJSON().Candidate)
		}
	})

	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		fmt.Printf("Gathering state: %s\n", state.String())
	})

	_, err = pc.CreateDataChannel("test", nil)
	if err != nil {
		fmt.Printf("CreateDataChannel err: %v\n", err)
		return
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		fmt.Printf("CreateOffer err: %v\n", err)
		return
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		fmt.Printf("SetLocalDescription err: %v\n", err)
		return
	}

	time.Sleep(5 * time.Second)
	fmt.Println("Done gathering test.")
}
