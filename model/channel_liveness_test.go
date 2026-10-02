package model

import (
	"net"
	"testing"
	"time"
)

func resetChannelLinkStore() {
	channelLinkMu.Lock()
	channelLinkStore = map[int]*channelLinkState{}
	channelLinkMu.Unlock()
}

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

func TestChannelLinkProbeAliveAndDead(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ch := &Channel{Id: 42, BaseURL: strPtr("http://" + addr)}

	alive, rtt := channelLinkProbe(ch, time.Second)
	if !alive {
		t.Fatalf("expected alive for %s", addr)
	}
	if rtt <= 0 {
		t.Fatalf("expected positive rtt, got %v", rtt)
	}

	_ = ln.Close()
	if alive, _ := channelLinkProbe(ch, time.Second); alive {
		t.Fatalf("expected dead after listener closed")
	}
}

func TestFilterLinkDeadKeepsLastCandidate(t *testing.T) {
	resetChannelLinkStore()
	orig := suspendUnreachableLink
	defer func() { suspendUnreachableLink = orig }()
	suspended := 0
	suspendUnreachableLink = func(group, model string, channelId int) error { suspended++; return nil }

	// 127.0.0.1:1 is closed -> connection refused quickly.
	ch := &Channel{Id: 100, Name: "dead", BaseURL: strPtr("http://127.0.0.1:1")}
	out := filterLinkDead("default", "embedding", []*Channel{ch})
	if len(out) != 1 {
		t.Fatalf("expected the last candidate to be kept, got %d", len(out))
	}
	if suspended != 0 {
		t.Fatalf("expected no suspend when it is the only candidate, got %d", suspended)
	}
}

func TestFilterLinkDeadDropsDeadAndSuspends(t *testing.T) {
	resetChannelLinkStore()
	orig := suspendUnreachableLink
	defer func() { suspendUnreachableLink = orig }()
	var suspended []int
	suspendUnreachableLink = func(group, model string, channelId int) error {
		suspended = append(suspended, channelId)
		return nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	aliveCh := &Channel{Id: 201, Name: "alive", BaseURL: strPtr("http://" + ln.Addr().String())}
	deadCh := &Channel{Id: 202, Name: "dead", BaseURL: strPtr("http://127.0.0.1:1")}

	out := filterLinkDead("default", "embedding", []*Channel{aliveCh, deadCh})
	if len(out) != 1 || out[0].Id != 201 {
		t.Fatalf("expected only the alive channel, got %+v", out)
	}
	if len(suspended) != 1 || suspended[0] != 202 {
		t.Fatalf("expected dead channel suspended once, got %v", suspended)
	}
}

func TestFilterLinkDeadSkipsHostnameEndpoints(t *testing.T) {
	resetChannelLinkStore()
	orig := suspendUnreachableLink
	defer func() { suspendUnreachableLink = orig }()
	called := false
	suspendUnreachableLink = func(group, model string, channelId int) error { called = true; return nil }

	ch := &Channel{Id: 300, Name: "public", BaseURL: strPtr("https://api.example.com")}
	out := filterLinkDead("default", "chat", []*Channel{ch})
	if len(out) != 1 {
		t.Fatalf("hostname endpoint must never be gated out")
	}
	if called {
		t.Fatalf("hostname endpoint must never be suspended")
	}
}
