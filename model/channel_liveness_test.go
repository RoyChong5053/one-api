package model

import (
	"testing"
	"time"
)

func strPtr(s string) *string { return &s }

func TestChannelLinkAddr(t *testing.T) {
	cases := []struct {
		name string
		base *string
		want string
	}{
		{"nil base", nil, ""},
		{"empty", strPtr(""), ""},
		{"ip with port", strPtr("http://192.168.10.1:11436"), "192.168.10.1:11436"},
		{"ip no port http", strPtr("http://192.168.10.1"), "192.168.10.1:80"},
		{"ip no port https", strPtr("https://10.0.0.5"), "10.0.0.5:443"},
		{"hostname skipped", strPtr("https://api.openai.com"), ""},
		{"path ignored", strPtr("http://127.0.0.1:11436/v1"), "127.0.0.1:11436"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := &Channel{Id: 1, BaseURL: tc.base}
			if got := channelLinkAddr(ch); got != tc.want {
				t.Fatalf("channelLinkAddr=%q want %q", got, tc.want)
			}
		})
	}
}

func TestIsLocalIPChannel(t *testing.T) {
	if IsLocalIPChannel(&Channel{BaseURL: strPtr("http://192.168.10.1:11436")}) != true {
		t.Fatal("expected IP-literal channel to be local")
	}
	if IsLocalIPChannel(&Channel{BaseURL: strPtr("https://api.openai.com")}) != false {
		t.Fatal("expected hostname channel not to be local")
	}
	if IsLocalIPChannel(nil) != false {
		t.Fatal("nil channel must not be local")
	}
}

func TestChannelLinkSnapshotRoundTrip(t *testing.T) {
	channelLinkMu.Lock()
	channelLinkStore = map[int]*channelLinkState{}
	channelLinkMu.Unlock()

	if _, _, checked := ChannelLinkSnapshot(999); checked {
		t.Fatal("expected unchecked before any measurement")
	}

	SetChannelLinkState(999, true, 3*time.Millisecond)
	alive, rttMs, checked := ChannelLinkSnapshot(999)
	if !checked || !alive || rttMs <= 0 {
		t.Fatalf("unexpected snapshot: checked=%v alive=%v rttMs=%v", checked, alive, rttMs)
	}
}
